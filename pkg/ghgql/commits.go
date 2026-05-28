package ghgql

import (
	"context"
	"encoding/base64"
	"fmt"
)

// FileChange is a single addition or deletion to apply to a commit.
type FileChange struct {
	Path string
	// Contents holds the file's bytes for additions; ignored for deletions.
	Contents []byte
	// Delete is true to delete the file; false to add or modify it.
	Delete bool
}

// CreateSignedCommitOnBranch creates a commit on the given branch via GitHub's `createCommitOnBranch`
// GraphQL mutation. The resulting commit is signed by GitHub's web-flow key (shown as Verified).
//
// `repo` is "owner/name", `branch` is the destination branch name, `expectedHeadOid` is the SHA
// the branch is expected to be at (for optimistic concurrency), `headline` and `body` make up the
// commit message, and `changes` lists the file additions and deletions to apply.
func (c *Client) CreateSignedCommitOnBranch(ctx context.Context, repo, branch, expectedHeadOid, headline, body string, changes []FileChange) (string, error) {
	additions := make([]FileAddition, 0, len(changes))
	deletions := make([]FileDeletion, 0, len(changes))
	for _, ch := range changes {
		if ch.Delete {
			deletions = append(deletions, FileDeletion{Path: ch.Path})
		} else {
			additions = append(additions, FileAddition{Path: ch.Path, Contents: base64.StdEncoding.EncodeToString(ch.Contents)})
		}
	}

	resp, err := createSignedCommitOnBranch(ctx, c.gql, repo, branch, expectedHeadOid, headline, body, additions, deletions)
	if err != nil {
		return "", fmt.Errorf("createCommitOnBranch mutation failed: %w", err)
	}
	return resp.CreateCommitOnBranch.Commit.Oid, nil
}
