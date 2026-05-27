package main

import (
	"context"
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSplitMessage(t *testing.T) {
	cases := []struct {
		in           string
		wantHeadline string
		wantBody     string
	}{
		{"Just a subject", "Just a subject", ""},
		{"Subject\n\nBody paragraph", "Subject", "Body paragraph"},
		{"Subject\n\nBody paragraph 1\n\nBody paragraph 2", "Subject", "Body paragraph 1\n\nBody paragraph 2"},
		{"Subject\nNo blank line", "Subject", "No blank line"},
		{"Subject\n\nBody\n\n(cherry picked from commit abc)\n", "Subject", "Body\n\n(cherry picked from commit abc)"},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			h, b := splitMessage(tc.in)
			require.Equal(t, tc.wantHeadline, h)
			require.Equal(t, tc.wantBody, b)
		})
	}
}

func TestAppendCoAuthor(t *testing.T) {
	t.Run("appends trailer when both name and email are set", func(t *testing.T) {
		out := appendCoAuthor("Subject\n\nBody", "Alice Example", "alice@example.com")
		require.Equal(t, "Subject\n\nBody\n\nCo-authored-by: Alice Example <alice@example.com>", out)
	})
	t.Run("omits trailer when name is empty", func(t *testing.T) {
		out := appendCoAuthor("Subject", "", "alice@example.com")
		require.Equal(t, "Subject", out)
	})
	t.Run("omits trailer when email is empty", func(t *testing.T) {
		out := appendCoAuthor("Subject", "Alice", "")
		require.Equal(t, "Subject", out)
	})
	t.Run("strips trailing newlines from message before appending", func(t *testing.T) {
		out := appendCoAuthor("Subject\n\n", "Alice", "a@x")
		require.Equal(t, "Subject\n\nCo-authored-by: Alice <a@x>", out)
	})
}

func TestBuildFileChanges(t *testing.T) {
	// Use a temp dir + chdir so os.ReadFile can find the added/modified files at the paths
	// reported by the mocked `git diff` output.
	tmpDir := t.TempDir()
	oldwd, _ := os.Getwd()
	require.NoError(t, os.Chdir(tmpDir))
	t.Cleanup(func() { _ = os.Chdir(oldwd) })

	require.NoError(t, os.WriteFile("added.txt", []byte("new content"), 0o644))
	require.NoError(t, os.WriteFile("modified.txt", []byte("updated content"), 0o644))
	require.NoError(t, os.MkdirAll("nested", 0o755))
	require.NoError(t, os.WriteFile("nested/file.txt", []byte("nested content"), 0o644))

	t.Run("empty diff yields no changes", func(t *testing.T) {
		runner := newMockRunner()
		runner.Outputs = map[string]string{
			"git diff --no-renames --name-status -z base HEAD": "",
		}
		changes, err := buildFileChanges(context.Background(), runner, "base", "HEAD")
		require.NoError(t, err)
		require.Empty(t, changes)
	})

	t.Run("addition reads file contents", func(t *testing.T) {
		runner := newMockRunner()
		runner.Outputs = map[string]string{
			"git diff --no-renames --name-status -z base HEAD": "A\x00added.txt\x00",
		}
		changes, err := buildFileChanges(context.Background(), runner, "base", "HEAD")
		require.NoError(t, err)
		require.Len(t, changes, 1)
		require.Equal(t, "added.txt", changes[0].Path)
		require.False(t, changes[0].Delete)
		require.Equal(t, []byte("new content"), changes[0].Contents)
	})

	t.Run("modification reads file contents", func(t *testing.T) {
		runner := newMockRunner()
		runner.Outputs = map[string]string{
			"git diff --no-renames --name-status -z base HEAD": "M\x00modified.txt\x00",
		}
		changes, err := buildFileChanges(context.Background(), runner, "base", "HEAD")
		require.NoError(t, err)
		require.Len(t, changes, 1)
		require.Equal(t, "modified.txt", changes[0].Path)
		require.Equal(t, []byte("updated content"), changes[0].Contents)
	})

	t.Run("deletion does not read file contents", func(t *testing.T) {
		runner := newMockRunner()
		runner.Outputs = map[string]string{
			"git diff --no-renames --name-status -z base HEAD": "D\x00gone.txt\x00",
		}
		changes, err := buildFileChanges(context.Background(), runner, "base", "HEAD")
		require.NoError(t, err)
		require.Len(t, changes, 1)
		require.Equal(t, "gone.txt", changes[0].Path)
		require.True(t, changes[0].Delete)
		require.Nil(t, changes[0].Contents)
	})

	t.Run("mixed entries preserve order from git output", func(t *testing.T) {
		runner := newMockRunner()
		runner.Outputs = map[string]string{
			"git diff --no-renames --name-status -z base HEAD": "M\x00modified.txt\x00D\x00gone.txt\x00A\x00nested/file.txt\x00",
		}
		changes, err := buildFileChanges(context.Background(), runner, "base", "HEAD")
		require.NoError(t, err)
		require.Len(t, changes, 3)
		require.Equal(t, "modified.txt", changes[0].Path)
		require.False(t, changes[0].Delete)
		require.Equal(t, "gone.txt", changes[1].Path)
		require.True(t, changes[1].Delete)
		require.Equal(t, "nested/file.txt", changes[2].Path)
		require.Equal(t, []byte("nested content"), changes[2].Contents)
	})

	t.Run("error when an added file is missing on disk", func(t *testing.T) {
		runner := newMockRunner()
		runner.Outputs = map[string]string{
			"git diff --no-renames --name-status -z base HEAD": "A\x00does-not-exist.txt\x00",
		}
		_, err := buildFileChanges(context.Background(), runner, "base", "HEAD")
		require.Error(t, err)
		require.Contains(t, err.Error(), "does-not-exist.txt")
	})

	t.Run("propagates runner error", func(t *testing.T) {
		runner := newErrorRunner(map[string]error{
			"git diff --no-renames --name-status -z base HEAD": os.ErrInvalid,
		})
		_, err := buildFileChanges(context.Background(), runner, "base", "HEAD")
		require.Error(t, err)
	})
}
