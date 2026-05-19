package main

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/go-github/v50/github"
	"github.com/grafana/grafana-github-actions-go/pkg/ghutil"
)

func BackportTargets(branches []*github.Branch, branchStrategy string, labels []string) ([]ghutil.Branch, error) {
	targets := []ghutil.Branch{}
	for _, label := range labels {
		if !strings.HasPrefix(label, "backport ") {
			continue
		}

		target, err := BackportTarget(label, branchStrategy, branches)
		if err != nil {
			return nil, fmt.Errorf("error getting target for backport label '%s': %w", label, err)
		}

		targets = append(targets, target)
	}

	return targets, nil
}

var (
	ErrorNotMerged = errors.New("pull request is not merged; nothing to do")
	ErrorBadAction = errors.New("unrecognized action")
	ErrorNoLabels  = errors.New("no labels found")
)

func BackportTargetsFromPayload(branches []*github.Branch, branchStrategy string, prInfo PrInfo) ([]ghutil.Branch, error) {
	if !prInfo.Pr.GetMerged() {
		return nil, ErrorNotMerged
	}

	if len(prInfo.Labels) == 0 {
		return nil, ErrorNoLabels
	}

	return BackportTargets(branches, branchStrategy, prInfo.Labels)
}

// BackportTarget finds the most appropriate base branch (target) given the backport label.
//
// This function accepts both the short form `backport v11.2.x` and the release-branch
// form `backport release-v2.8`, and finds the most recent `release-` branch that matches.
//
// If branchStrategy is 'mimir', the expected label string is `backport {name}`,
// with {name} being the exact name of the target.
func BackportTarget(label string, branchStrategy string, branches []*github.Branch) (ghutil.Branch, error) {
	if branchStrategy == "mimir" {
		return mimirBackportTarget(label, branches)
	}

	version := strings.TrimSpace(strings.TrimPrefix(label, "backport"))
	version = strings.TrimPrefix(version, "release-")
	labelString := strings.ReplaceAll(version, "x", "0")
	major, minor, _ := ghutil.MajorMinorPatch(labelString)

	return ghutil.MostRecentBranch(major, minor, branches)
}

func mimirBackportTarget(label string, branches []*github.Branch) (ghutil.Branch, error) {
	name := strings.TrimSpace(strings.TrimPrefix(label, "backport"))
	branch, found := ghutil.FindBranchByName(name, branches)
	if !found {
		return branch, fmt.Errorf("branch not found: %s", name)
	}
	return branch, nil
}

func MergeBase(ctx context.Context, client *github.RepositoriesService, owner, repo, base, head string) (*github.Commit, error) {
	comp, _, err := client.CompareCommits(ctx, owner, repo, base, head, &github.ListOptions{})
	if err != nil {
		return nil, err
	}

	return comp.MergeBaseCommit.Commit, nil
}
