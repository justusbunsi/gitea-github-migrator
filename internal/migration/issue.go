package migration

import (
	"context"
	"fmt"
	"sort"
	"strconv"

	"code.gitea.io/sdk/gitea"
)

// GetAllGiteaIssues has two modes: "actual retrieval" and "count".
// In "actual retrieval" mode, it fetches all available issues from Gitea (excluding pull requests).
// In "count" mode it fetches the first page and returns only the "x-total-count" header.
func (e *Entry) GetAllGiteaIssues(ctx context.Context, countMode bool) ([]*gitea.Issue, int, error) {
	var issues []*gitea.Issue
	var totalCount *int

	logMessage := "retrieving Gitea issues"

	opts := gitea.ListIssueOption{
		State: gitea.StateAll,
		Type:  gitea.IssueTypeIssue,
	}
	if countMode {
		opts.PageSize = 1
		opts.Page = 1
		logMessage = "retrieving Gitea issue total count"
		e.Logger.Debug(logMessage)
	} else {
		e.Logger.Info(logMessage)
	}
	for {
		if err := ctx.Err(); err != nil {
			return nil, 0, fmt.Errorf("%s: %v", logMessage, err)
		}

		result, resp, err := e.giteaClient.ListRepoIssues(e.GiteaOwner, e.GiteaRepo, opts)
		if err != nil {
			return nil, 0, fmt.Errorf("%s: %v", logMessage, err)
		}

		if totalCount == nil {
			c, err := strconv.Atoi(resp.Header.Get("x-total-count"))
			if err != nil {
				return nil, 0, fmt.Errorf("%s: unable to retrieve total count: %v", logMessage, err)
			}
			totalCount = &c
		}

		if countMode {
			return nil, *totalCount, nil
		}

		issues = append(issues, result...)

		if resp.NextPage == 0 {
			break
		}

		opts.Page = resp.NextPage
	}

	// Gitea issue listing API does not support sorting, so we sort ourselves descending (aka oldest first).
	sort.Slice(issues, func(i, j int) bool {
		return issues[i].Created.Before(issues[j].Created)
	})

	return issues, *totalCount, nil
}
