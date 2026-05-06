package cache

import (
	"encoding/json"
	"errors"
	"os"
	"slices"
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
	giteaPullReviewsCacheType
)

type CommentCacheEntry struct {
	GiteaHash       string
	GitHubHash      string
	GitHubCommentID int64
}

type ReviewCacheEntry struct {
	ContentHash     string
	GitHubCommentID int64
}

type ItemCacheEntry struct {
	ContentHash    string
	GitHubItemID   int64
	CommentEntries map[int64]CommentCacheEntry
	ReviewEntries  map[int64]ReviewCacheEntry
}

type ReleaseCacheEntry struct {
	ContentHash     string
	GitHubReleaseID int64
}

type persistedReleaseEntry struct {
	GiteaID         int64  `json:"gitea_id"`
	ContentHash     string `json:"content_hash"`
	GitHubReleaseID int64  `json:"github_release_id"`
}

type Cache struct {
	mutex             *sync.RWMutex
	store             map[uint8]map[string]any
	completed         map[string]map[int64]ItemCacheEntry
	failed            map[string]map[int64]struct{}
	completedReleases map[string]map[int64]ReleaseCacheEntry
	failedReleases    map[string]map[int64]struct{}
}

type persistedCommentEntry struct {
	GiteaCommentID  int64  `json:"gitea_id"`
	GiteaHash       string `json:"gitea_hash"`
	GitHubHash      string `json:"github_hash"`
	GitHubCommentID int64  `json:"github_id"`
}

type persistedReviewEntry struct {
	GiteaReviewID   int64  `json:"gitea_id"`
	ContentHash     string `json:"content_hash"`
	GitHubCommentID int64  `json:"github_id"`
}

type persistedItemEntry struct {
	GiteaID        int64                   `json:"gitea_id"`
	ContentHash    string                  `json:"content_hash"`
	GitHubID       int64                   `json:"github_id"`
	CommentEntries []persistedCommentEntry `json:"comment_entries"`
	ReviewEntries  []persistedReviewEntry  `json:"review_entries"`
}

type persistedCache struct {
	CompletedItems    map[string][]persistedItemEntry    `json:"completed_items"`
	FailedItems       map[string][]int64                 `json:"failed_items"`
	CompletedReleases map[string][]persistedReleaseEntry `json:"completed_releases"`
	FailedReleases    map[string][]int64                 `json:"failed_releases"`
}

func New() *Cache {
	store := make(map[uint8]map[string]any)
	store[githubPullRequestCacheType] = make(map[string]any)
	store[githubSearchResultsCacheType] = make(map[string]any)
	store[githubUserCacheType] = make(map[string]any)
	store[giteaUserCacheType] = make(map[string]any)
	store[giteaPullReviewsCacheType] = make(map[string]any)

	return &Cache{
		mutex:             new(sync.RWMutex),
		store:             store,
		completed:         make(map[string]map[int64]ItemCacheEntry),
		failed:            make(map[string]map[int64]struct{}),
		completedReleases: make(map[string]map[int64]ReleaseCacheEntry),
		failedReleases:    make(map[string]map[int64]struct{}),
	}
}

func (c Cache) IsCompleted(repoKey string, index int64) bool {
	c.mutex.RLock()
	defer c.mutex.RUnlock()
	if indices, ok := c.completed[repoKey]; ok {
		_, done := indices[index]
		return done
	}
	return false
}

func (c Cache) GetCompletedEntry(repoKey string, index int64) (ItemCacheEntry, bool) {
	c.mutex.RLock()
	defer c.mutex.RUnlock()
	if indices, ok := c.completed[repoKey]; ok {
		entry, done := indices[index]
		return entry, done
	}
	return ItemCacheEntry{}, false
}

func (c Cache) MarkCompleted(repoKey string, index int64, entry ItemCacheEntry) {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	if _, ok := c.completed[repoKey]; !ok {
		c.completed[repoKey] = make(map[int64]ItemCacheEntry)
	}
	c.completed[repoKey][index] = entry
	if indices, ok := c.failed[repoKey]; ok {
		delete(indices, index)
	}
}

func (c Cache) MarkRepoKnown(repoKey string) {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	if _, ok := c.completed[repoKey]; !ok {
		c.completed[repoKey] = make(map[int64]ItemCacheEntry)
	}
}

func (c Cache) IsKnownRepo(repoKey string) bool {
	c.mutex.RLock()
	defer c.mutex.RUnlock()
	if _, ok := c.completed[repoKey]; ok {
		return true
	}
	if _, ok := c.failed[repoKey]; ok {
		return true
	}
	if _, ok := c.completedReleases[repoKey]; ok {
		return true
	}
	if _, ok := c.failedReleases[repoKey]; ok {
		return true
	}
	return false
}

func (c Cache) IsFailed(repoKey string, index int64) bool {
	c.mutex.RLock()
	defer c.mutex.RUnlock()
	if indices, ok := c.failed[repoKey]; ok {
		_, failed := indices[index]
		return failed
	}
	return false
}

func (c Cache) MarkFailed(repoKey string, index int64) {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	if _, ok := c.failed[repoKey]; !ok {
		c.failed[repoKey] = make(map[int64]struct{})
	}
	c.failed[repoKey][index] = struct{}{}
}

func (c Cache) IsReleaseCompleted(repoKey string, index int64) bool {
	c.mutex.RLock()
	defer c.mutex.RUnlock()
	if indices, ok := c.completedReleases[repoKey]; ok {
		_, done := indices[index]
		return done
	}
	return false
}

func (c Cache) GetCompletedReleaseEntry(repoKey string, index int64) (ReleaseCacheEntry, bool) {
	c.mutex.RLock()
	defer c.mutex.RUnlock()
	if indices, ok := c.completedReleases[repoKey]; ok {
		entry, done := indices[index]
		return entry, done
	}
	return ReleaseCacheEntry{}, false
}

func (c Cache) MarkReleaseCompleted(repoKey string, index int64, entry ReleaseCacheEntry) {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	if _, ok := c.completedReleases[repoKey]; !ok {
		c.completedReleases[repoKey] = make(map[int64]ReleaseCacheEntry)
	}
	c.completedReleases[repoKey][index] = entry
	if indices, ok := c.failedReleases[repoKey]; ok {
		delete(indices, index)
	}
}

func (c Cache) IsReleaseFailed(repoKey string, index int64) bool {
	c.mutex.RLock()
	defer c.mutex.RUnlock()
	if indices, ok := c.failedReleases[repoKey]; ok {
		_, failed := indices[index]
		return failed
	}
	return false
}

func (c Cache) MarkReleaseFailed(repoKey string, index int64) {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	if _, ok := c.failedReleases[repoKey]; !ok {
		c.failedReleases[repoKey] = make(map[int64]struct{})
	}
	c.failedReleases[repoKey][index] = struct{}{}
}

func (c Cache) PurgeRepo(repoKey string) {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	delete(c.completed, repoKey)
	delete(c.failed, repoKey)
	delete(c.completedReleases, repoKey)
	delete(c.failedReleases, repoKey)
}

func (c Cache) LoadFromFile(path string) error {
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

	for repoKey, entries := range p.CompletedItems {
		if _, ok := c.completed[repoKey]; !ok {
			c.completed[repoKey] = make(map[int64]ItemCacheEntry)
		}
		for _, pe := range entries {
			ce := ItemCacheEntry{
				ContentHash:  pe.ContentHash,
				GitHubItemID: pe.GitHubID,
			}
			if len(pe.CommentEntries) > 0 {
				ce.CommentEntries = make(map[int64]CommentCacheEntry, len(pe.CommentEntries))
				for _, pce := range pe.CommentEntries {
					ce.CommentEntries[pce.GiteaCommentID] = CommentCacheEntry{
						GiteaHash:       pce.GiteaHash,
						GitHubHash:      pce.GitHubHash,
						GitHubCommentID: pce.GitHubCommentID,
					}
				}
			}
			if len(pe.ReviewEntries) > 0 {
				ce.ReviewEntries = make(map[int64]ReviewCacheEntry, len(pe.ReviewEntries))
				for _, pre := range pe.ReviewEntries {
					ce.ReviewEntries[pre.GiteaReviewID] = ReviewCacheEntry{
						ContentHash:     pre.ContentHash,
						GitHubCommentID: pre.GitHubCommentID,
					}
				}
			}
			c.completed[repoKey][pe.GiteaID] = ce
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

	for repoKey, entries := range p.CompletedReleases {
		if _, ok := c.completedReleases[repoKey]; !ok {
			c.completedReleases[repoKey] = make(map[int64]ReleaseCacheEntry)
		}
		for _, pe := range entries {
			c.completedReleases[repoKey][pe.GiteaID] = ReleaseCacheEntry{
				ContentHash:     pe.ContentHash,
				GitHubReleaseID: pe.GitHubReleaseID,
			}
		}
	}
	for repoKey, indices := range p.FailedReleases {
		if _, ok := c.failedReleases[repoKey]; !ok {
			c.failedReleases[repoKey] = make(map[int64]struct{})
		}
		for _, idx := range indices {
			c.failedReleases[repoKey][idx] = struct{}{}
		}
	}

	return nil
}

func (c Cache) SaveToFile(path string) error {
	c.mutex.RLock()
	defer c.mutex.RUnlock()

	p := persistedCache{
		CompletedItems:    make(map[string][]persistedItemEntry),
		FailedItems:       make(map[string][]int64),
		CompletedReleases: make(map[string][]persistedReleaseEntry),
		FailedReleases:    make(map[string][]int64),
	}

	for repoKey, entries := range c.completed {
		list := make([]persistedItemEntry, 0, len(entries))
		for idx, entry := range entries {
			pe := persistedItemEntry{
				GiteaID:     idx,
				ContentHash: entry.ContentHash,
				GitHubID:    entry.GitHubItemID,
			}
			if len(entry.CommentEntries) > 0 {
				pe.CommentEntries = make([]persistedCommentEntry, 0, len(entry.CommentEntries))
				for commentID, ce := range entry.CommentEntries {
					pe.CommentEntries = append(pe.CommentEntries, persistedCommentEntry{
						GiteaCommentID:  commentID,
						GiteaHash:       ce.GiteaHash,
						GitHubHash:      ce.GitHubHash,
						GitHubCommentID: ce.GitHubCommentID,
					})
				}
				slices.SortFunc(pe.CommentEntries, func(a, b persistedCommentEntry) int { return int(a.GiteaCommentID - b.GiteaCommentID) })
			}
			if len(entry.ReviewEntries) > 0 {
				pe.ReviewEntries = make([]persistedReviewEntry, 0, len(entry.ReviewEntries))
				for reviewID, re := range entry.ReviewEntries {
					pe.ReviewEntries = append(pe.ReviewEntries, persistedReviewEntry{
						GiteaReviewID:   reviewID,
						ContentHash:     re.ContentHash,
						GitHubCommentID: re.GitHubCommentID,
					})
				}
				slices.SortFunc(pe.ReviewEntries, func(a, b persistedReviewEntry) int { return int(a.GiteaReviewID - b.GiteaReviewID) })
			}
			list = append(list, pe)
		}
		slices.SortFunc(list, func(a, b persistedItemEntry) int { return int(a.GiteaID - b.GiteaID) })
		p.CompletedItems[repoKey] = list
	}
	for repoKey, indices := range c.failed {
		list := make([]int64, 0, len(indices))
		for idx := range indices {
			list = append(list, idx)
		}
		slices.Sort(list)
		p.FailedItems[repoKey] = list
	}

	for repoKey, entries := range c.completedReleases {
		list := make([]persistedReleaseEntry, 0, len(entries))
		for idx, entry := range entries {
			list = append(list, persistedReleaseEntry{
				GiteaID:         idx,
				ContentHash:     entry.ContentHash,
				GitHubReleaseID: entry.GitHubReleaseID,
			})
		}
		slices.SortFunc(list, func(a, b persistedReleaseEntry) int { return int(a.GiteaID - b.GiteaID) })
		p.CompletedReleases[repoKey] = list
	}
	for repoKey, indices := range c.failedReleases {
		list := make([]int64, 0, len(indices))
		for idx := range indices {
			list = append(list, idx)
		}
		slices.Sort(list)
		p.FailedReleases[repoKey] = list
	}

	data, err := json.Marshal(p)
	if err != nil {
		return err
	}

	return os.WriteFile(path, data, 0o644)
}

func (c Cache) GetGithubPullRequest(query string) *github.PullRequest {
	c.mutex.RLock()
	defer c.mutex.RUnlock()
	if v, ok := c.store[githubPullRequestCacheType][query]; ok {
		return h.Pointer(v.(github.PullRequest))
	}
	return nil
}

func (c Cache) SetGithubPullRequest(query string, result github.PullRequest) {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	c.store[githubPullRequestCacheType][query] = result
}

func (c Cache) GetGithubSearchResults(query string) *github.IssuesSearchResult {
	c.mutex.RLock()
	defer c.mutex.RUnlock()
	if v, ok := c.store[githubSearchResultsCacheType][query]; ok {
		return h.Pointer(v.(github.IssuesSearchResult))
	}
	return nil
}

func (c Cache) SetGithubSearchResults(query string, result github.IssuesSearchResult) {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	c.store[githubSearchResultsCacheType][query] = result
}

func (c Cache) GetGithubUser(username string) *github.User {
	c.mutex.RLock()
	defer c.mutex.RUnlock()
	if v, ok := c.store[giteaUserCacheType][username]; ok {
		return h.Pointer(v.(github.User))
	}
	return nil
}

func (c Cache) SetGithubUser(username string, user github.User) {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	c.store[giteaUserCacheType][username] = user
}

func (c Cache) GetGiteaPullReviews(key string) []*gitea.PullReview {
	c.mutex.RLock()
	defer c.mutex.RUnlock()
	if v, ok := c.store[giteaPullReviewsCacheType][key]; ok {
		return v.([]*gitea.PullReview)
	}
	return nil
}

func (c Cache) SetGiteaPullReviews(key string, reviews []*gitea.PullReview) {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	c.store[giteaPullReviewsCacheType][key] = reviews
}

func (c Cache) GetGiteaUser(username string) *gitea.User {
	c.mutex.RLock()
	defer c.mutex.RUnlock()
	if v, ok := c.store[githubUserCacheType][username]; ok {
		return h.Pointer(v.(gitea.User))
	}
	return nil
}

func (c Cache) SetGiteaUser(username string, user gitea.User) {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	c.store[githubUserCacheType][username] = user
}
