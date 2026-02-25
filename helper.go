package main

import (
	"context"
	"fmt"
	"time"

	"code.gitea.io/sdk/gitea"
	"github.com/google/go-github/v74/github"
)

func getGithubPullRequest(ctx context.Context, org, repo string, prNumber int) (*github.PullRequest, error) {
	var err error
	cacheToken := fmt.Sprintf("%s/%s/%d", org, repo, prNumber)
	pullRequest := cache.getGithubPullRequest(cacheToken)
	if pullRequest == nil {
		logger.Debug("retrieving pull request details", "owner", org, "repo", repo, "pr_number", prNumber)
		pullRequest, _, err = gh.PullRequests.Get(ctx, org, repo, prNumber)
		if err != nil {
			return nil, fmt.Errorf("retrieving pull request: %v", err)
		}

		if pullRequest == nil {
			return nil, fmt.Errorf("nil pull request was returned: %d", prNumber)
		}

		logger.Trace("caching pull request details", "owner", org, "repo", repo, "pr_number", prNumber)
		cache.setGithubPullRequest(cacheToken, *pullRequest)
	}

	return pullRequest, nil
}

func getGithubSearchResults(ctx context.Context, query string) (*github.IssuesSearchResult, error) {
	var err error
	result := cache.getGithubSearchResults(query)
	if result == nil {
		logger.Debug("performing search", "query", query)
		result, _, err = gh.Search.Issues(ctx, query, nil)
		if err != nil {
			return nil, fmt.Errorf("performing issue search: %v", err)
		}

		if result == nil {
			return nil, fmt.Errorf("nil search result was returned for query: %s", query)
		}

		logger.Trace("caching GitHub search result", "query", query)
		cache.setGithubSearchResults(query, *result)
	}

	return result, nil
}

func getGithubUser(ctx context.Context, username string) (*github.User, error) {
	var err error
	user := cache.getGithubUser(username)
	if user == nil {
		logger.Debug("retrieving user details", "username", username)
		if user, _, err = gh.Users.Get(ctx, username); err != nil {
			return nil, err
		}

		if user == nil {
			return nil, fmt.Errorf("nil user was returned: %s", username)
		}

		logger.Trace("caching GitHub user", "username", username)
		cache.setGithubUser(username, *user)
	}

	if user.Type == nil {
		return nil, fmt.Errorf("unable to determine whether owner is a user or organisatition: %s", username)
	}

	return user, nil
}

// TODO: pre-cache user list result to reduce overall Gitea API requests
// TODO: unify getGiteaUser and getGiteaOrganization
func getGiteaUser(username string) (*gitea.User, error) {
	user := cache.getGiteaUser(username)
	if user == nil {
		logger.Debug("retrieving user details", "username", username)
		var users []*gitea.User
		opts := gitea.AdminListUsersOptions{}
		for {
			result, resp, err := gi.AdminListUsers(opts)
			if err != nil {
				return nil, fmt.Errorf("retrieving gitea user list: %v", err)
			}

			users = append(users, result...)

			if resp.NextPage == 0 {
				break
			}

			opts.ListOptions.Page = resp.NextPage
		}

		for _, user = range users {
			if user != nil && user.UserName == username {
				logger.Trace("caching Gitea user", "username", username)
				cache.setGiteaUser(username, *user)

				return user, nil
			}
		}

		return nil, fmt.Errorf("gitea user not found: %s", username)
	}

	return user, nil
}

func pointer[T any](v T) *T {
	return &v
}

func roundDuration(d, r time.Duration) time.Duration {
	if r <= 0 {
		return d
	}
	neg := d < 0
	if neg {
		d = -d
	}
	if m := d % r; m+m < r {
		d = d - m
	} else {
		d = d + r - m
	}
	if neg {
		return -d
	}
	return d
}
