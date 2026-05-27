package ghutil

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/go-github/v50/github"
)

// SignedCommitGitClient is the subset of *github.GitService that
// PublishSignedCommit needs. Declared as an interface so it can be faked
// in tests.
type SignedCommitGitClient interface {
	GetRef(ctx context.Context, owner, repo, ref string) (*github.Reference, *github.Response, error)
	GetCommit(ctx context.Context, owner, repo, sha string) (*github.Commit, *github.Response, error)
	CreateBlob(ctx context.Context, owner, repo string, blob *github.Blob) (*github.Blob, *github.Response, error)
	CreateTree(ctx context.Context, owner, repo, baseTree string, entries []*github.TreeEntry) (*github.Tree, *github.Response, error)
	CreateCommit(ctx context.Context, owner, repo string, commit *github.Commit) (*github.Commit, *github.Response, error)
	CreateRef(ctx context.Context, owner, repo string, ref *github.Reference) (*github.Reference, *github.Response, error)
}

// GitRunner runs `git` and returns its stdout.
type GitRunner interface {
	Output(ctx context.Context, args ...string) ([]byte, error)
}

type execGitRunner struct {
	dir string
}

// NewGitRunner returns a GitRunner that runs `git` in the given working directory.
// If dir is "", git inherits the current working directory.
func NewGitRunner(dir string) GitRunner {
	return &execGitRunner{dir: dir}
}

func (r *execGitRunner) Output(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = r.dir
	stderr := &bytes.Buffer{}
	cmd.Stderr = stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git %s: %w (stderr: %s)", strings.Join(args, " "), err, stderr.String())
	}
	return out, nil
}

// SignedCommitOpts describes the change to publish as a server-signed commit.
type SignedCommitOpts struct {
	Owner      string
	Repository string

	// BaseBranch is the branch the new commit will be parented to. Its tip
	// is read via the API to derive the parent commit and base tree.
	BaseBranch string

	// HeadBranch is the new branch ref to create (without the
	// "refs/heads/" prefix).
	HeadBranch string

	// WorkingDir is the local git working tree to read file contents from.
	// It does not affect the GitRunner, which carries its own directory.
	// Defaults to ".".
	WorkingDir string

	// LocalCommitRef is the local commit whose changes will be published.
	// LocalCommitRef^ is used as the diff base. Defaults to "HEAD".
	LocalCommitRef string
}

// PublishSignedCommit publishes the change in opts.LocalCommitRef to GitHub
// as a server-signed (Verified) commit, using only the Git Data API — no
// `git push` required.
//
// The committer is intentionally omitted on the create-commit call so that
// GitHub fills it in with the authenticated identity. When the token belongs
// to a GitHub App (the workflow's GITHUB_TOKEN qualifies), GitHub signs the
// commit with its own key and it shows up as Verified. The local commit's
// author and message are preserved on the published commit.
func PublishSignedCommit(ctx context.Context, log *slog.Logger, gh SignedCommitGitClient, runner GitRunner, opts SignedCommitOpts) (*github.Reference, error) {
	if opts.Owner == "" || opts.Repository == "" || opts.BaseBranch == "" || opts.HeadBranch == "" {
		return nil, errors.New("Owner, Repository, BaseBranch and HeadBranch are required")
	}
	if opts.LocalCommitRef == "" {
		opts.LocalCommitRef = "HEAD"
	}
	if opts.WorkingDir == "" {
		opts.WorkingDir = "."
	}

	author, message, err := readLocalCommit(ctx, runner, opts.LocalCommitRef)
	if err != nil {
		return nil, fmt.Errorf("error reading local commit: %w", err)
	}

	changes, err := readLocalDiff(ctx, runner, opts.LocalCommitRef)
	if err != nil {
		return nil, fmt.Errorf("error reading local diff: %w", err)
	}

	baseRef, _, err := gh.GetRef(ctx, opts.Owner, opts.Repository, "refs/heads/"+opts.BaseBranch)
	if err != nil {
		return nil, fmt.Errorf("error fetching base ref: %w", err)
	}
	baseSHA := baseRef.GetObject().GetSHA()

	baseCommit, _, err := gh.GetCommit(ctx, opts.Owner, opts.Repository, baseSHA)
	if err != nil {
		return nil, fmt.Errorf("error fetching base commit: %w", err)
	}
	baseTreeSHA := baseCommit.GetTree().GetSHA()

	entries, err := buildTreeEntries(ctx, log, gh, opts, changes)
	if err != nil {
		return nil, err
	}

	tree, _, err := gh.CreateTree(ctx, opts.Owner, opts.Repository, baseTreeSHA, entries)
	if err != nil {
		return nil, fmt.Errorf("error creating tree: %w", err)
	}

	commit, _, err := gh.CreateCommit(ctx, opts.Owner, opts.Repository, &github.Commit{
		Message: github.String(message),
		Tree:    &github.Tree{SHA: tree.SHA},
		Parents: []*github.Commit{{SHA: github.String(baseSHA)}},
		Author:  author,
	})
	if err != nil {
		return nil, fmt.Errorf("error creating commit: %w", err)
	}

	ref, _, err := gh.CreateRef(ctx, opts.Owner, opts.Repository, &github.Reference{
		Ref:    github.String("refs/heads/" + opts.HeadBranch),
		Object: &github.GitObject{SHA: commit.SHA},
	})
	if err != nil {
		return nil, fmt.Errorf("error creating ref: %w", err)
	}
	return ref, nil
}

type changeKind int

const (
	changeAddModify changeKind = iota
	changeDelete
)

type change struct {
	kind changeKind
	path string
	// mode and blobSHA are populated from `git ls-tree` for AddModify entries.
	mode    string
	blobSHA string
}

func buildTreeEntries(ctx context.Context, log *slog.Logger, gh SignedCommitGitClient, opts SignedCommitOpts, changes []change) ([]*github.TreeEntry, error) {
	entries := make([]*github.TreeEntry, 0, len(changes))
	for _, c := range changes {
		if c.kind == changeDelete {
			entries = append(entries, &github.TreeEntry{
				Path: github.String(c.path),
				Mode: github.String("100644"),
				Type: github.String("blob"),
				// SHA: nil -> the entry is removed from the tree.
			})
			continue
		}

		// Submodule: include the gitlink SHA directly; no blob to upload.
		if c.mode == "160000" {
			entries = append(entries, &github.TreeEntry{
				Path: github.String(c.path),
				Mode: github.String("160000"),
				Type: github.String("commit"),
				SHA:  github.String(c.blobSHA),
			})
			continue
		}

		content, err := readPathContent(opts.WorkingDir, c.path, c.mode)
		if err != nil {
			return nil, fmt.Errorf("error reading %q: %w", c.path, err)
		}

		log.Debug("uploading blob", "path", c.path, "bytes", len(content), "mode", c.mode)
		blob, _, err := gh.CreateBlob(ctx, opts.Owner, opts.Repository, &github.Blob{
			Content:  github.String(base64.StdEncoding.EncodeToString(content)),
			Encoding: github.String("base64"),
		})
		if err != nil {
			return nil, fmt.Errorf("error creating blob for %q: %w", c.path, err)
		}

		entries = append(entries, &github.TreeEntry{
			Path: github.String(c.path),
			Mode: github.String(c.mode),
			Type: github.String("blob"),
			SHA:  blob.SHA,
		})
	}
	return entries, nil
}

func readLocalCommit(ctx context.Context, runner GitRunner, ref string) (*github.CommitAuthor, string, error) {
	// Use a NUL separator between the author tuple and the (potentially
	// multi-line) message so we can split deterministically.
	out, err := runner.Output(ctx, "show", "-s", "--format=%an%n%ae%n%aI%x00%B", ref)
	if err != nil {
		return nil, "", err
	}
	parts := bytes.SplitN(out, []byte{0}, 2)
	if len(parts) != 2 {
		return nil, "", fmt.Errorf("unexpected git show output: %q", string(out))
	}
	headerLines := strings.SplitN(string(parts[0]), "\n", 3)
	if len(headerLines) < 3 {
		return nil, "", fmt.Errorf("unexpected git show header: %q", string(parts[0]))
	}
	ts, err := time.Parse(time.RFC3339, strings.TrimSpace(headerLines[2]))
	if err != nil {
		return nil, "", fmt.Errorf("parsing author date: %w", err)
	}
	author := &github.CommitAuthor{
		Name:  github.String(strings.TrimSpace(headerLines[0])),
		Email: github.String(strings.TrimSpace(headerLines[1])),
		Date:  &github.Timestamp{Time: ts},
	}
	message := strings.TrimRight(string(parts[1]), "\n")
	return author, message, nil
}

func readLocalDiff(ctx context.Context, runner GitRunner, ref string) ([]change, error) {
	out, err := runner.Output(ctx, "diff", "--name-status", "-z", "-M", ref+"^", ref)
	if err != nil {
		return nil, err
	}
	changes := parseNameStatusZ(string(out))

	var addModifyPaths []string
	for _, c := range changes {
		if c.kind == changeAddModify {
			addModifyPaths = append(addModifyPaths, c.path)
		}
	}
	if len(addModifyPaths) == 0 {
		return changes, nil
	}

	modes, shas, err := readLocalTree(ctx, runner, ref, addModifyPaths)
	if err != nil {
		return nil, fmt.Errorf("reading file modes: %w", err)
	}
	for i := range changes {
		if changes[i].kind != changeAddModify {
			continue
		}
		mode, ok := modes[changes[i].path]
		if !ok {
			return nil, fmt.Errorf("no tree entry for %q at %s", changes[i].path, ref)
		}
		changes[i].mode = mode
		changes[i].blobSHA = shas[changes[i].path]
	}
	return changes, nil
}

// parseNameStatusZ parses the output of `git diff --name-status -z -M`.
// The stream alternates between a status token and one path (or two paths,
// for renames/copies), each terminated by a NUL byte.
func parseNameStatusZ(out string) []change {
	tokens := strings.Split(out, "\x00")
	for len(tokens) > 0 && tokens[len(tokens)-1] == "" {
		tokens = tokens[:len(tokens)-1]
	}

	var changes []change
	for i := 0; i < len(tokens); {
		status := tokens[i]
		i++
		switch {
		case len(status) == 0:
			// Skip stray empty tokens defensively.
		case status[0] == 'R', status[0] == 'C':
			if i+1 >= len(tokens) {
				return changes
			}
			oldPath, newPath := tokens[i], tokens[i+1]
			i += 2
			changes = append(changes,
				change{kind: changeDelete, path: oldPath},
				change{kind: changeAddModify, path: newPath},
			)
		case status[0] == 'D':
			if i >= len(tokens) {
				return changes
			}
			changes = append(changes, change{kind: changeDelete, path: tokens[i]})
			i++
		default:
			// A, M, T, U etc. — treat as add/modify of a single path.
			if i >= len(tokens) {
				return changes
			}
			changes = append(changes, change{kind: changeAddModify, path: tokens[i]})
			i++
		}
	}
	return changes
}

func readLocalTree(ctx context.Context, runner GitRunner, ref string, paths []string) (map[string]string, map[string]string, error) {
	args := append([]string{"ls-tree", "-z", ref, "--"}, paths...)
	out, err := runner.Output(ctx, args...)
	if err != nil {
		return nil, nil, err
	}
	modes := map[string]string{}
	shas := map[string]string{}
	for _, rec := range strings.Split(string(out), "\x00") {
		if rec == "" {
			continue
		}
		tab := strings.IndexByte(rec, '\t')
		if tab < 0 {
			return nil, nil, fmt.Errorf("unexpected ls-tree record: %q", rec)
		}
		meta, path := rec[:tab], rec[tab+1:]
		fields := strings.Fields(meta)
		if len(fields) < 3 {
			return nil, nil, fmt.Errorf("unexpected ls-tree meta: %q", meta)
		}
		modes[path] = fields[0]
		shas[path] = fields[2]
	}
	return modes, shas, nil
}

func readPathContent(wd, path, mode string) ([]byte, error) {
	abs := filepath.Join(wd, path)
	if mode == "120000" {
		target, err := os.Readlink(abs)
		if err != nil {
			return nil, err
		}
		return []byte(target), nil
	}
	return os.ReadFile(abs)
}
