package migration

import (
	"context"
	"fmt"
	"sort"
	"strconv"

	"code.gitea.io/sdk/gitea"
)

// GetAllGiteaReleases has two modes: "actual retrieval" and "count".
// In "actual retrieval" mode, it fetches all available releases from Gitea.
// In "count" mode it fetches the first page and returns only the "x-total-count" header.
func (e *Entry) GetAllGiteaReleases(ctx context.Context, countMode bool) ([]*gitea.Release, int, error) {
	var releases []*gitea.Release
	var totalCount *int

	logMessage := "retrieving Gitea releases"

	opts := gitea.ListReleasesOptions{}
	if countMode {
		opts.PageSize = 1
		opts.Page = 1
		logMessage = "retrieving Gitea release total count"
		e.Logger.Debug(logMessage)
	} else {
		e.Logger.Info(logMessage)
	}

	for {
		// Check for context cancellation
		if err := ctx.Err(); err != nil {
			return nil, 0, fmt.Errorf("%s: %v", logMessage, err)
		}

		result, resp, err := e.giteaClient.ListReleases(e.GiteaOwner, e.GiteaRepo, opts)
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

		releases = append(releases, result...)

		if resp.NextPage == 0 {
			break
		}

		opts.Page = resp.NextPage
	}

	sort.Slice(releases, func(i, j int) bool {
		return releases[i].CreatedAt.Before(releases[j].CreatedAt)
	})

	return releases, *totalCount, nil
}
