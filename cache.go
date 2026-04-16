package main

import (
	"sync"

	"code.gitea.io/sdk/gitea"
	"github.com/google/go-github/v74/github"
	h "github.com/justusbunsi/gitea-github-migrator/internal/helpers"
)

const (
	githubPullRequestCacheType uint8 = iota
	githubSearchResultsCacheType
	githubUserCacheType
	giteaUserCacheType
)

type objectCache struct {
	mutex *sync.RWMutex
	store map[uint8]map[string]any
}

func newObjectCache() *objectCache {
	store := make(map[uint8]map[string]any)
	store[githubPullRequestCacheType] = make(map[string]any)
	store[githubSearchResultsCacheType] = make(map[string]any)
	store[githubUserCacheType] = make(map[string]any)
	store[giteaUserCacheType] = make(map[string]any)

	return &objectCache{
		mutex: new(sync.RWMutex),
		store: store,
	}
}

func (c objectCache) getGithubPullRequest(query string) *github.PullRequest {
	c.mutex.RLock()
	defer c.mutex.RUnlock()
	if v, ok := c.store[githubPullRequestCacheType][query]; ok {
		return h.Pointer(v.(github.PullRequest))
	}
	return nil
}

func (c objectCache) setGithubPullRequest(query string, result github.PullRequest) {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	c.store[githubPullRequestCacheType][query] = result
}

func (c objectCache) getGithubSearchResults(query string) *github.IssuesSearchResult {
	c.mutex.RLock()
	defer c.mutex.RUnlock()
	if v, ok := c.store[githubSearchResultsCacheType][query]; ok {
		return h.Pointer(v.(github.IssuesSearchResult))
	}
	return nil
}

func (c objectCache) setGithubSearchResults(query string, result github.IssuesSearchResult) {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	c.store[githubSearchResultsCacheType][query] = result
}

func (c objectCache) getGithubUser(username string) *github.User {
	c.mutex.RLock()
	defer c.mutex.RUnlock()
	if v, ok := c.store[giteaUserCacheType][username]; ok {
		return h.Pointer(v.(github.User))
	}
	return nil
}

func (c objectCache) setGithubUser(username string, user github.User) {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	c.store[giteaUserCacheType][username] = user
}

func (c objectCache) getGiteaUser(username string) *gitea.User {
	c.mutex.RLock()
	defer c.mutex.RUnlock()
	if v, ok := c.store[githubUserCacheType][username]; ok {
		return h.Pointer(v.(gitea.User))
	}
	return nil
}

func (c objectCache) setGiteaUser(username string, user gitea.User) {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	c.store[githubUserCacheType][username] = user
}
