package main

import (
	"encoding/json"
	"errors"
	"os"
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
	mutex     *sync.RWMutex
	store     map[uint8]map[string]any
	completed map[string]map[int64]struct{}
	failed    map[string]map[int64]struct{}
}

type persistedCache struct {
	CompletedItems map[string][]int64 `json:"completed_items"`
	FailedItems    map[string][]int64 `json:"failed_items"`
}

func newObjectCache() *objectCache {
	store := make(map[uint8]map[string]any)
	store[githubPullRequestCacheType] = make(map[string]any)
	store[githubSearchResultsCacheType] = make(map[string]any)
	store[githubUserCacheType] = make(map[string]any)
	store[giteaUserCacheType] = make(map[string]any)

	return &objectCache{
		mutex:     new(sync.RWMutex),
		store:     store,
		completed: make(map[string]map[int64]struct{}),
		failed:    make(map[string]map[int64]struct{}),
	}
}

func (c objectCache) isCompleted(repoKey string, index int64) bool {
	c.mutex.RLock()
	defer c.mutex.RUnlock()
	if indices, ok := c.completed[repoKey]; ok {
		_, done := indices[index]
		return done
	}
	return false
}

func (c objectCache) markCompleted(repoKey string, index int64) {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	if _, ok := c.completed[repoKey]; !ok {
		c.completed[repoKey] = make(map[int64]struct{})
	}
	c.completed[repoKey][index] = struct{}{}
	if indices, ok := c.failed[repoKey]; ok {
		delete(indices, index)
	}
}

func (c objectCache) isKnownRepo(repoKey string) bool {
	c.mutex.RLock()
	defer c.mutex.RUnlock()
	if indices, ok := c.completed[repoKey]; ok && len(indices) > 0 {
		return true
	}
	if indices, ok := c.failed[repoKey]; ok && len(indices) > 0 {
		return true
	}
	return false
}

func (c objectCache) isFailed(repoKey string, index int64) bool {
	c.mutex.RLock()
	defer c.mutex.RUnlock()
	if indices, ok := c.failed[repoKey]; ok {
		_, failed := indices[index]
		return failed
	}
	return false
}

func (c objectCache) markFailed(repoKey string, index int64) {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	if _, ok := c.failed[repoKey]; !ok {
		c.failed[repoKey] = make(map[int64]struct{})
	}
	c.failed[repoKey][index] = struct{}{}
}

func (c objectCache) purgeRepo(repoKey string) {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	delete(c.completed, repoKey)
	delete(c.failed, repoKey)
}

func (c objectCache) loadFromFile(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}

	var p persistedCache
	if err := json.Unmarshal(data, &p); err != nil {
		return err
	}

	c.mutex.Lock()
	defer c.mutex.Unlock()

	for repoKey, indices := range p.CompletedItems {
		if _, ok := c.completed[repoKey]; !ok {
			c.completed[repoKey] = make(map[int64]struct{})
		}
		for _, idx := range indices {
			c.completed[repoKey][idx] = struct{}{}
		}
	}
	for repoKey, indices := range p.FailedItems {
		if _, ok := c.failed[repoKey]; !ok {
			c.failed[repoKey] = make(map[int64]struct{})
		}
		for _, idx := range indices {
			c.failed[repoKey][idx] = struct{}{}
		}
	}

	return nil
}

func (c objectCache) saveToFile(path string) error {
	c.mutex.RLock()
	defer c.mutex.RUnlock()

	p := persistedCache{
		CompletedItems: make(map[string][]int64),
		FailedItems:    make(map[string][]int64),
	}

	for repoKey, indices := range c.completed {
		list := make([]int64, 0, len(indices))
		for idx := range indices {
			list = append(list, idx)
		}
		p.CompletedItems[repoKey] = list
	}
	for repoKey, indices := range c.failed {
		list := make([]int64, 0, len(indices))
		for idx := range indices {
			list = append(list, idx)
		}
		p.FailedItems[repoKey] = list
	}

	data, err := json.Marshal(p)
	if err != nil {
		return err
	}

	return os.WriteFile(path, data, 0o644)
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
