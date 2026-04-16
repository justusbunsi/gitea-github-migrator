package migration

import (
	"context"
	"fmt"
	"strconv"

	"code.gitea.io/sdk/gitea"
)

// GetAllGiteaPullRequests has two modes: "actual retrieval" and "count".
// In "actual retrieval" mode, it fetches all available PRs from Gitea.
// In "count" mode it fetches the first page and returns only the "x-total-count" header.
func (e *Entry) GetAllGiteaPullRequests(ctx context.Context, countMode bool) ([]*gitea.PullRequest, int, error) {
	var pullRequests []*gitea.PullRequest
	var totalCount *int

	logMessage := "retrieving Gitea pull requests"

	opts := gitea.ListPullRequestsOptions{
		State: gitea.StateAll,
		Sort:  "oldest",
	}
	if countMode {
		opts.PageSize = 1
		opts.Page = 1
		logMessage = "retrieving Gitea pull request total count"
		e.Logger.Debug(logMessage)
	} else {
		e.Logger.Info(logMessage)
	}
	for {
		// Check for context cancellation
		if err := ctx.Err(); err != nil {
			return nil, 0, fmt.Errorf("%s: %v", logMessage, err)
		}

		result, resp, err := e.giteaClient.ListRepoPullRequests(e.GiteaOwner, e.GiteaRepo, opts)
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

		pullRequests = append(pullRequests, result...)

		if resp.NextPage == 0 {
			break
		}

		opts.Page = resp.NextPage
	}

	return pullRequests, *totalCount, nil
}
