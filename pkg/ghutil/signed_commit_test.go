package ghutil

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-github/v50/github"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseNameStatusZ(t *testing.T) {
	t.Run("add modify delete", func(t *testing.T) {
		// Layout: "A\0added\0M\0modified\0D\0removed\0"
		in := "A\x00added\x00M\x00modified\x00D\x00removed\x00"
		got := parseNameStatusZ(in)
		assert.Equal(t, []change{
			{kind: changeAddModify, path: "added"},
			{kind: changeAddModify, path: "modified"},
			{kind: changeDelete, path: "removed"},
		}, got)
	})

	t.Run("rename is delete plus add", func(t *testing.T) {
		in := "R100\x00old/name.txt\x00new/name.txt\x00M\x00other\x00"
		got := parseNameStatusZ(in)
		assert.Equal(t, []change{
			{kind: changeDelete, path: "old/name.txt"},
			{kind: changeAddModify, path: "new/name.txt"},
			{kind: changeAddModify, path: "other"},
		}, got)
	})

	t.Run("empty", func(t *testing.T) {
		assert.Nil(t, parseNameStatusZ(""))
	})
}

// stubGitRunner returns canned stdout for prefix-matched argv strings.
type stubGitRunner struct {
	out map[string][]byte
	err map[string]error
}

func (s *stubGitRunner) Output(_ context.Context, args ...string) ([]byte, error) {
	key := strings.Join(args, " ")
	if e, ok := s.err[key]; ok {
		return nil, e
	}
	for k, v := range s.out {
		if key == k || strings.HasPrefix(key, k+" ") {
			return v, nil
		}
	}
	return nil, fmt.Errorf("stubGitRunner: no canned output for %q", key)
}

// recordingGitClient implements SignedCommitGitClient. It records inputs and
// returns canned outputs so tests can assert the exact wire shape.
type recordingGitClient struct {
	baseRef    *github.Reference
	baseCommit *github.Commit

	blobs        []*github.Blob
	createdTree  *github.Tree
	treeBaseSHA  string
	treeEntries  []*github.TreeEntry
	createdCmt   *github.Commit
	commitInput  *github.Commit
	createdRef   *github.Reference
	refInputName string
}

func (r *recordingGitClient) GetRef(_ context.Context, _, _, _ string) (*github.Reference, *github.Response, error) {
	return r.baseRef, nil, nil
}
func (r *recordingGitClient) GetCommit(_ context.Context, _, _, _ string) (*github.Commit, *github.Response, error) {
	return r.baseCommit, nil, nil
}
func (r *recordingGitClient) CreateBlob(_ context.Context, _, _ string, blob *github.Blob) (*github.Blob, *github.Response, error) {
	r.blobs = append(r.blobs, blob)
	sha := fmt.Sprintf("blob-sha-%d", len(r.blobs))
	return &github.Blob{SHA: github.String(sha)}, nil, nil
}
func (r *recordingGitClient) CreateTree(_ context.Context, _, _ string, baseTree string, entries []*github.TreeEntry) (*github.Tree, *github.Response, error) {
	r.treeBaseSHA = baseTree
	r.treeEntries = entries
	r.createdTree = &github.Tree{SHA: github.String("tree-sha")}
	return r.createdTree, nil, nil
}
func (r *recordingGitClient) CreateCommit(_ context.Context, _, _ string, c *github.Commit) (*github.Commit, *github.Response, error) {
	r.commitInput = c
	r.createdCmt = &github.Commit{SHA: github.String("commit-sha")}
	return r.createdCmt, nil, nil
}
func (r *recordingGitClient) CreateRef(_ context.Context, _, _ string, ref *github.Reference) (*github.Reference, *github.Response, error) {
	r.refInputName = ref.GetRef()
	r.createdRef = &github.Reference{
		Ref:    ref.Ref,
		Object: &github.GitObject{SHA: ref.GetObject().SHA},
	}
	return r.createdRef, nil, nil
}

func TestPublishSignedCommit(t *testing.T) {
	wd := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(wd, "added.txt"), []byte("hello\n"), 0o644))
	require.NoError(t, os.MkdirAll(filepath.Join(wd, "sub"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(wd, "sub", "renamed.txt"), []byte("after rename\n"), 0o644))

	runner := &stubGitRunner{
		out: map[string][]byte{
			"show -s --format=%an%n%ae%n%aI%x00%B HEAD": []byte(
				"Jane Doe\njane@example.com\n2024-01-02T03:04:05Z\x00" +
					"Subject line\n\nBody line\n",
			),
			"diff --name-status -z -M HEAD^ HEAD": []byte(
				"A\x00added.txt\x00R100\x00sub/old.txt\x00sub/renamed.txt\x00D\x00deleted.txt\x00",
			),
			"ls-tree -z HEAD -- added.txt sub/renamed.txt": []byte(
				"100644 blob aaaaaaaa\tadded.txt\x00100755 blob bbbbbbbb\tsub/renamed.txt\x00",
			),
		},
	}

	gh := &recordingGitClient{
		baseRef: &github.Reference{
			Ref:    github.String("refs/heads/main"),
			Object: &github.GitObject{SHA: github.String("base-commit-sha")},
		},
		baseCommit: &github.Commit{
			SHA:  github.String("base-commit-sha"),
			Tree: &github.Tree{SHA: github.String("base-tree-sha")},
		},
	}

	ref, err := PublishSignedCommit(context.Background(), slog.Default(), gh, runner, SignedCommitOpts{
		Owner:      "grafana",
		Repository: "grafana",
		BaseBranch: "main",
		HeadBranch: "feature/x",
		WorkingDir: wd,
	})
	require.NoError(t, err)

	require.NotNil(t, ref)
	assert.Equal(t, "refs/heads/feature/x", ref.GetRef())
	assert.Equal(t, "refs/heads/feature/x", gh.refInputName)
	assert.Equal(t, "commit-sha", ref.GetObject().GetSHA())

	require.Equal(t, "base-tree-sha", gh.treeBaseSHA)

	require.Len(t, gh.blobs, 2)
	added, err := base64.StdEncoding.DecodeString(gh.blobs[0].GetContent())
	require.NoError(t, err)
	assert.Equal(t, "hello\n", string(added))
	assert.Equal(t, "base64", gh.blobs[0].GetEncoding())
	renamed, err := base64.StdEncoding.DecodeString(gh.blobs[1].GetContent())
	require.NoError(t, err)
	assert.Equal(t, "after rename\n", string(renamed))

	require.Len(t, gh.treeEntries, 4)
	byPath := map[string]*github.TreeEntry{}
	for _, e := range gh.treeEntries {
		byPath[e.GetPath()] = e
	}

	addedEntry := byPath["added.txt"]
	require.NotNil(t, addedEntry)
	assert.Equal(t, "100644", addedEntry.GetMode())
	assert.Equal(t, "blob", addedEntry.GetType())
	assert.Equal(t, "blob-sha-1", addedEntry.GetSHA())

	renamedEntry := byPath["sub/renamed.txt"]
	require.NotNil(t, renamedEntry)
	assert.Equal(t, "100755", renamedEntry.GetMode(), "executable bit must be carried over from ls-tree")
	assert.Equal(t, "blob-sha-2", renamedEntry.GetSHA())

	for _, p := range []string{"sub/old.txt", "deleted.txt"} {
		e := byPath[p]
		require.NotNil(t, e, "missing delete entry for %s", p)
		assert.Nil(t, e.SHA, "delete entry for %s must have nil SHA so the API removes it", p)
	}

	require.NotNil(t, gh.commitInput)
	assert.Equal(t, "Subject line\n\nBody line", gh.commitInput.GetMessage())
	assert.Equal(t, "Jane Doe", gh.commitInput.GetAuthor().GetName())
	assert.Equal(t, "jane@example.com", gh.commitInput.GetAuthor().GetEmail())
	assert.Nil(t, gh.commitInput.Committer,
		"Committer must be left nil so GitHub fills in the bot identity and signs the commit")
	require.Len(t, gh.commitInput.Parents, 1)
	assert.Equal(t, "base-commit-sha", gh.commitInput.Parents[0].GetSHA())
	assert.Equal(t, "tree-sha", gh.commitInput.Tree.GetSHA())
}

func TestPublishSignedCommit_PropagatesGitErrors(t *testing.T) {
	runner := &stubGitRunner{
		err: map[string]error{
			"show -s --format=%an%n%ae%n%aI%x00%B HEAD": errors.New("boom"),
		},
	}
	_, err := PublishSignedCommit(context.Background(), slog.Default(), &recordingGitClient{}, runner, SignedCommitOpts{
		Owner:      "o",
		Repository: "r",
		BaseBranch: "main",
		HeadBranch: "h",
		WorkingDir: t.TempDir(),
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "boom")
}
