package migration

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"

	"code.gitea.io/sdk/gitea"
	"github.com/google/go-github/v74/github"
	"github.com/justusbunsi/gitea-github-migrator/internal/cache"
	"github.com/justusbunsi/gitea-github-migrator/internal/constants"
	h "github.com/justusbunsi/gitea-github-migrator/internal/helpers"
)

func (e *Entry) GetAllPullRequestReviews(ctx context.Context, giteaItemId int64) ([]*gitea.PullReview, error) {
	cacheKey := fmt.Sprintf("%s/%s/%d", e.GiteaOwner, e.GiteaRepo, giteaItemId)
	if cached := e.Cache.GetGiteaPullReviews(cacheKey); cached != nil {
		return cached, nil
	}

	var giteaReviews []*gitea.PullReview
	opts := gitea.ListPullReviewsOptions{}

	e.Logger.Debug("retrieving Gitea pull request reviews", "item_id", giteaItemId)
	for {
		// Check for context cancellation
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("retrieving gitea pull request reviews: %v", err)
		}

		result, resp, err := e.giteaClient.ListPullReviews(e.GiteaOwner, e.GiteaRepo, giteaItemId, opts)
		if err != nil {
			return nil, fmt.Errorf("listing gitea pull request reviews: %v", err)
		}

		giteaReviews = append(giteaReviews, result...)

		if resp.NextPage == 0 {
			break
		}

		opts.Page = resp.NextPage
	}

	e.Logger.Trace("caching pull request reviews", "item_id", giteaItemId)
	e.Cache.SetGiteaPullReviews(cacheKey, giteaReviews)
	return giteaReviews, nil
}

func (e *Entry) GetAllPullRequestReviewComments(ctx context.Context, giteaItemId int64, review *gitea.PullReview) ([]*gitea.PullReviewComment, error) {
	if review.CodeCommentsCount == 0 {
		return nil, nil
	}

	// Check for context cancellation
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("retrieving gitea pull request review comments: %v", err)
	}

	e.Logger.Debug("retrieving Gitea pull request review comments", "item_id", giteaItemId, "review_id", review.ID)
	result, _, err := e.giteaClient.ListPullReviewComments(e.GiteaOwner, e.GiteaRepo, giteaItemId, review.ID)
	if err != nil {
		return nil, fmt.Errorf("listing gitea pull request review comments: %v", err)
	}

	return result, nil
}

type reviewCommentGroup struct {
	path     string
	diffHunk string
	comments []*gitea.PullReviewComment
}

func formatReviewComment(c *gitea.PullReviewComment) string {
	author := h.GetGitHubAccountReference(c.Reviewer)
	date := c.Created.Format(constants.DateFormat)

	normalized := strings.ReplaceAll(c.Body, "\r\n", "\n")
	lines := strings.Split(normalized, "\n")
	prefixed := make([]string, 0, len(lines))
	for _, line := range lines {
		if line == "" {
			prefixed = append(prefixed, ">")
		} else {
			prefixed = append(prefixed, "> "+line)
		}
	}

	return fmt.Sprintf("> **%s** commented **%s**\n%s", author, date, strings.Join(prefixed, "\n"))
}

func buildReviewConversations(comments []*gitea.PullReviewComment) string {
	if len(comments) == 0 {
		return "_No conversations_"
	}

	// Sort by (path, diff_hunk, created) so that group_by(path, diff_hunk) preserves
	// comment thread order (oldest first within each thread).
	sort.Slice(comments, func(i, j int) bool {
		if comments[i].Path != comments[j].Path {
			return comments[i].Path < comments[j].Path
		}
		if comments[i].DiffHunk != comments[j].DiffHunk {
			return comments[i].DiffHunk < comments[j].DiffHunk
		}
		return comments[i].Created.Before(comments[j].Created)
	})

	var groups []reviewCommentGroup
	for _, c := range comments {
		if len(groups) == 0 || groups[len(groups)-1].path != c.Path || groups[len(groups)-1].diffHunk != c.DiffHunk {
			groups = append(groups, reviewCommentGroup{path: c.Path, diffHunk: c.DiffHunk})
		}
		groups[len(groups)-1].comments = append(groups[len(groups)-1].comments, c)
	}

	detailsBlocks := make([]string, 0, len(groups))
	for _, group := range groups {
		resolved := false
		for _, c := range group.comments {
			if c.Resolver != nil {
				resolved = true
				break
			}
		}

		openAttr := " open"
		resolvedSuffix := ""
		if resolved {
			openAttr = ""
			resolvedSuffix = " [resolved]"
		}

		commitID := group.comments[0].CommitID

		commentBlocks := make([]string, 0, len(group.comments))
		for _, c := range group.comments {
			commentBlocks = append(commentBlocks, formatReviewComment(c))
		}

		block := fmt.Sprintf("<details%s>\n<summary>%s (%s)%s</summary>\n\n```diff\n%s\n```\n\n%s\n\n</details>",
			openAttr, group.path, commitID, resolvedSuffix, group.diffHunk, strings.Join(commentBlocks, "\n>\n"))
		detailsBlocks = append(detailsBlocks, block)
	}

	return strings.Join(detailsBlocks, "\n\n---\n\n")
}

func (e *Entry) buildReviewBody(review *gitea.PullReview, comments []*gitea.PullReviewComment, giteaItemId, githubItemId int64) (body, contentHash string) {
	reviewer := h.GetGitHubAccountReference(review.Reviewer)

	var stateLabel string
	switch review.State {
	case gitea.ReviewStateApproved:
		stateLabel = ":white_check_mark: approved"
	case gitea.ReviewStateRequestChanges:
		stateLabel = ":x: changes requested"
	default:
		stateLabel = ":speech_balloon: commented"
	}

	header := fmt.Sprintf(`> [!IMPORTANT]
> This code review was migrated from Gitea
>
> |      |      |
> | ---- | ---- |
> | **Original Reviewer** | %s |
> | **Review ID** | %d |
> | **Date Originally Reviewed** | %s |
> | **Original Review State** | %s |
> |      |      |
>`, reviewer, review.ID, review.Submitted.Format(constants.DateFormat), stateLabel)

	reviewBody := review.Body
	if strings.TrimSpace(reviewBody) == "" {
		reviewBody = "_No comment_"
	}

	conversations := buildReviewConversations(comments)

	body = fmt.Sprintf("%s\n\n## Original review comment\n\n%s\n\n## Original review conversations\n\n%s",
		header, reviewBody, conversations)

	if len(body) > constants.GithubBodyLimit {
		e.Logger.Warn("review comment was truncated due to platform limits", "gitea_item", giteaItemId, "github_item", githubItemId, "review_id", review.ID)
		body = strings.ReplaceAll(body, "This code review was migrated from Gitea", "This code review was migrated from Gitea **and was truncated due to platform limits**")
		body = body[:constants.GithubBodyLimit] + "..."
	}

	sum := md5.Sum([]byte(body))
	return body, hex.EncodeToString(sum[:])
}

func (e *Entry) MigratePullRequestReviews(ctx context.Context, giteaItemId int64, githubItemId int64) (map[int64]cache.ReviewCacheEntry, error) {
	reviews, err := e.GetAllPullRequestReviews(ctx, giteaItemId)
	if err != nil {
		return nil, err
	}

	if len(reviews) == 0 {
		return nil, nil
	}

	sort.Slice(reviews, func(i, j int) bool { return reviews[i].ID < reviews[j].ID })

	// We don't care about gitea.ReviewStatePending or gitea.ReviewStateRequestReview state, as those do not provide any meaningful migration data.
	filtered := make([]*gitea.PullReview, 0, len(reviews))
	for _, r := range reviews {
		if r.State == gitea.ReviewStateApproved || r.State == gitea.ReviewStateRequestChanges || r.State == gitea.ReviewStateComment {
			filtered = append(filtered, r)
		}
	}
	reviews = filtered

	var reviewEntries map[int64]cache.ReviewCacheEntry
	if cached, ok := e.Cache.GetCompletedEntry(e.GetCacheID(), giteaItemId); ok {
		reviewEntries = cached.ReviewEntries
	}
	if reviewEntries == nil {
		reviewEntries = make(map[int64]cache.ReviewCacheEntry)
	}

	e.Logger.Debug("migrating pull request reviews from Gitea to GitHub", "item_id", githubItemId, "count", len(reviews))

	// cache-file mode: use cached GitHub comment IDs for direct create/update.
	if len(reviewEntries) > 0 {
		for _, review := range reviews {
			if review == nil {
				continue
			}
			// Check for context cancellation
			if err := ctx.Err(); err != nil {
				return nil, fmt.Errorf("migrating pull request reviews: %v", err)
			}

			comments, err := e.GetAllPullRequestReviewComments(ctx, giteaItemId, review)
			if err != nil {
				return nil, err
			}

			reviewBody, contentHash := e.buildReviewBody(review, comments, giteaItemId, githubItemId)

			if cached, ok := reviewEntries[review.ID]; ok {
				if cached.ContentHash == contentHash {
					e.Logger.Trace("existing review comment is up-to-date (hash match)", "gitea_item", giteaItemId, "github_item", githubItemId, "review_id", review.ID)
					continue
				}
				e.Logger.Debug("updating review comment (hash mismatch)", "gitea_item", giteaItemId, "github_item", githubItemId, "review_id", review.ID)
				if _, _, err := e.githubClient.Issues.EditComment(ctx, e.GitHubOwner, e.GitHubRepo, cached.GitHubCommentID, &github.IssueComment{Body: &reviewBody}); err != nil {
					return nil, fmt.Errorf("updating review comment: %v", err)
				}
				if err := h.SleepWithContext(ctx, constants.GithubApiPauseBetweenMutativeRequests); err != nil {
					return nil, err
				}
				reviewEntries[review.ID] = cache.ReviewCacheEntry{ContentHash: contentHash, GitHubCommentID: cached.GitHubCommentID}
				continue
			}

			e.Logger.Debug("creating review comment", "gitea_item", giteaItemId, "github_item", githubItemId, "review_id", review.ID)
			created, _, err := e.githubClient.Issues.CreateComment(ctx, e.GitHubOwner, e.GitHubRepo, int(githubItemId), &github.IssueComment{Body: &reviewBody})
			if err != nil {
				return nil, fmt.Errorf("creating review comment for gitea item #%d (#%d): %v", giteaItemId, githubItemId, err)
			}
			if created.GetID() == 0 {
				return nil, fmt.Errorf("creating review comment for gitea item #%d (#%d): GitHub returned ID 0", giteaItemId, githubItemId)
			}
			if err := h.SleepWithContext(ctx, constants.GithubApiPauseBetweenMutativeRequests); err != nil {
				return nil, err
			}
			reviewEntries[review.ID] = cache.ReviewCacheEntry{ContentHash: contentHash, GitHubCommentID: created.GetID()}
		}
		return reviewEntries, nil
	}

	// non-cache-file mode / first-run: fetch GitHub comments and match by Review ID marker.
	e.Logger.Debug("retrieving GitHub comments for review matching", "gitea_item", giteaItemId, "github_item", githubItemId)
	githubComments, _, err := e.githubClient.Issues.ListComments(ctx, e.GitHubOwner, e.GitHubRepo, int(githubItemId), &github.IssueListCommentsOptions{Sort: h.Pointer("created"), Direction: h.Pointer("asc")})
	if err != nil {
		return nil, fmt.Errorf("listing github comments for review matching: %v", err)
	}

	for _, review := range reviews {
		if review == nil {
			continue
		}
		// Check for context cancellation
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("migrating pull request reviews: %v", err)
		}

		comments, err := e.GetAllPullRequestReviewComments(ctx, giteaItemId, review)
		if err != nil {
			return nil, err
		}

		reviewBody, contentHash := e.buildReviewBody(review, comments, giteaItemId, githubItemId)
		reviewIDMarker := fmt.Sprintf("**Review ID** | %d", review.ID)

		foundExisting := false
		for _, githubComment := range githubComments {
			if githubComment == nil {
				continue
			}
			if !strings.Contains(githubComment.GetBody(), reviewIDMarker) {
				continue
			}

			foundExisting = true
			if githubComment.Body == nil || *githubComment.Body != reviewBody {
				e.Logger.Debug("updating review comment", "gitea_item", giteaItemId, "github_item", githubItemId, "review_id", review.ID)
				if _, _, err = e.githubClient.Issues.EditComment(ctx, e.GitHubOwner, e.GitHubRepo, githubComment.GetID(), &github.IssueComment{Body: &reviewBody}); err != nil {
					return nil, fmt.Errorf("updating review comment: %v", err)
				}
				if err = h.SleepWithContext(ctx, constants.GithubApiPauseBetweenMutativeRequests); err != nil {
					return nil, err
				}
			} else {
				e.Logger.Trace("existing review comment is up-to-date", "gitea_item", giteaItemId, "github_item", githubItemId, "review_id", review.ID)
			}
			if githubComment.GetID() == 0 {
				return nil, fmt.Errorf("review %d on gitea item #%d: GitHub returned ID 0", review.ID, giteaItemId)
			}
			reviewEntries[review.ID] = cache.ReviewCacheEntry{ContentHash: contentHash, GitHubCommentID: githubComment.GetID()}
			break
		}

		if !foundExisting {
			e.Logger.Debug("creating review comment", "gitea_item", giteaItemId, "github_item", githubItemId, "review_id", review.ID)
			created, _, err := e.githubClient.Issues.CreateComment(ctx, e.GitHubOwner, e.GitHubRepo, int(githubItemId), &github.IssueComment{Body: &reviewBody})
			if err != nil {
				return nil, fmt.Errorf("creating review comment for gitea item #%d (#%d): %v", giteaItemId, githubItemId, err)
			}
			if created.GetID() == 0 {
				return nil, fmt.Errorf("creating review comment for gitea item #%d (#%d): GitHub returned ID 0", giteaItemId, githubItemId)
			}
			if err = h.SleepWithContext(ctx, constants.GithubApiPauseBetweenMutativeRequests); err != nil {
				return nil, err
			}
			reviewEntries[review.ID] = cache.ReviewCacheEntry{ContentHash: contentHash, GitHubCommentID: created.GetID()}
		}
	}

	return reviewEntries, nil
}
