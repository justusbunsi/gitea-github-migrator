package main

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"code.gitea.io/sdk/gitea"
	"github.com/google/go-github/v74/github"
)

func getGithubPullRequest(ctx context.Context, org, repo string, prNumber int) (*github.PullRequest, error) {
	var err error
	cacheToken := fmt.Sprintf("%s/%s/%d", org, repo, prNumber)
	pullRequest := cache.getGithubPullRequest(cacheToken)
	if pullRequest == nil {
		logger.Debug("retrieving pull request details", "github", fmt.Sprintf("%s/%s", org, repo), "pr_number", prNumber)
		pullRequest, _, err = gh.PullRequests.Get(ctx, org, repo, prNumber)
		if err != nil {
			return nil, fmt.Errorf("retrieving pull request: %v", err)
		}

		if pullRequest == nil {
			return nil, fmt.Errorf("nil pull request was returned: %d", prNumber)
		}

		logger.Trace("caching pull request details", "github", fmt.Sprintf("%s/%s", org, repo), "pr_number", prNumber)
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
		logger.Debug("retrieving GitHub user details", "username", username)
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
		return nil, fmt.Errorf("unable to determine whether owner is a user or organisation: %s", username)
	}

	return user, nil
}

// getAllGiteaPullRequests has two modes: "actual retrieval" and "count".
// In "actual retrieval" mode, it fetches all available PRs from Gitea.
// In "count" mode it fetches the first page and returns only the "x-total-count" header.
func getAllGiteaPullRequests(ctx context.Context, owner, repo string, countMode bool) ([]*gitea.PullRequest, int, error) {
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
		logger.Debug(logMessage, "gitea_owner", owner, "gitea_repo", repo)
	} else {
		logger.Info(logMessage, "gitea_owner", owner, "gitea_repo", repo)
	}
	for {
		// Check for context cancellation
		if err := ctx.Err(); err != nil {
			return nil, 0, fmt.Errorf("%s: %v", logMessage, err)
		}

		result, resp, err := gi.ListRepoPullRequests(owner, repo, opts)
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

// getGitHubAccountReference reads the Gitea account website property for any GitHub account references.
// If found a GitHub account reference, that reference is used. Fallback is the original Gitea account name.
func getGitHubAccountReference(giteaUser *gitea.User) string {
	if giteaUser.Website != "" && strings.Index(giteaUser.Website, "https://github.com/") == 0 {
		return "@" + strings.TrimPrefix(strings.ToLower(giteaUser.Website), "https://github.com/")
	}

	return giteaUser.UserName
}

func parseProjectSlugs(proj []string) ([]string, []string, error) {
	if len(proj) != 2 {
		return nil, nil, fmt.Errorf("too many fields")
	}

	delimPosition := strings.LastIndex(proj[0], "/")
	giteaPath := []string{
		proj[0][:delimPosition],
		proj[0][delimPosition+1:],
	}
	githubPath := strings.Split(proj[1], "/")

	if len(giteaPath) != 2 {
		return nil, nil, fmt.Errorf("invalid Gitea project: %s", proj[0])
	}
	if len(githubPath) != 2 {
		return nil, nil, fmt.Errorf("invalid GitHub project: %s", proj[1])
	}

	return giteaPath, githubPath, nil
}
