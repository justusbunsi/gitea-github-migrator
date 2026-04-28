package migration

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"strings"

	"code.gitea.io/sdk/gitea"
	"github.com/google/go-github/v74/github"
	"github.com/justusbunsi/gitea-github-migrator/internal/constants"
	h "github.com/justusbunsi/gitea-github-migrator/internal/helpers"
)

// CommentCacheEntry holds the content hash and GitHub comment ID for a single migrated comment.
type CommentCacheEntry struct {
	GiteaHash       string
	GitHubHash      string
	GitHubCommentID int64
}

func (e *Entry) buildCommentBody(comment *gitea.Comment, giteaItemId int64, githubItemId int64) (body, giteaHash, githubHash string) {
	body = fmt.Sprintf(`> [!NOTE]
> This comment was migrated from Gitea
>
> |      |      |
> | ---- | ---- |
> | **Original Author** | %[1]s |
> | **Comment ID** | %[2]d |
> | **Date Originally Created** | %[3]s |
> |      |      |
>

## Original Comment

%[4]s`, h.GetGitHubAccountReference(comment.Poster), comment.ID, comment.Created.Format(constants.DateFormat), comment.Body)
	if len(body) > constants.GithubBodyLimit {
		e.Logger.Warn("comment was truncated due to platform limits", "gitea_item", giteaItemId, "github_item", githubItemId, "comment_id", comment.ID)
		body = strings.ReplaceAll(body, "This comment was migrated from Gitea", "This comment was migrated from Gitea **and was truncated due to platform limits**")
		body = body[:constants.GithubBodyLimit] + "..."
	}
	giteaSum := md5.Sum([]byte(comment.Body))
	githubSum := md5.Sum([]byte(body))
	return body, hex.EncodeToString(giteaSum[:]), hex.EncodeToString(githubSum[:])
}

// MigrateComments migrates comments from a Gitea item to the corresponding GitHub item.
//
// commentEntries is the cached map of Gitea comment ID → CommentCacheEntry.
// When non-empty (cache-file mode) each comment is compared by GiteaHash and GitHubHash and updated in-place via its cached GitHub ID,
// avoiding a full GitHub comment list fetch.
// When empty (non cache-file mode) the existing GitHub comments are fetched and matched by body content, populating the map for future runs.
//
// The returned map is the updated commentEntries and must be persisted by the caller.
func (e *Entry) MigrateComments(ctx context.Context, giteaItemId int64, githubItemId int64, commentEntries map[int64]CommentCacheEntry) (map[int64]CommentCacheEntry, error) {
	if commentEntries == nil {
		commentEntries = make(map[int64]CommentCacheEntry)
	}

	var giteaComments []*gitea.Comment
	opts := gitea.ListIssueCommentOptions{}

	e.Logger.Debug("retrieving Gitea comments", "item_id", giteaItemId)
	for {
		// Check for context cancellation
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("retrieving gitea comments: %v", err)
		}

		result, resp, err := e.giteaClient.ListIssueComments(e.GiteaOwner, e.GiteaRepo, giteaItemId, opts)
		if err != nil {
			return nil, fmt.Errorf("listing gitea comments: %v", err)
		}

		giteaComments = append(giteaComments, result...)

		if resp.NextPage == 0 {
			break
		}

		opts.Page = resp.NextPage
	}

	e.Logger.Info("migrating comments from Gitea to GitHub", "item_id", githubItemId, "count", len(giteaComments))

	if len(giteaComments) == 0 {
		// We don't need to request GitHub API if there are no comments to be migrated at all. Those secondary rate limit points can be safed.
		return commentEntries, nil
	}

	// cache-file mode (on migration resume): use cached GitHubCommentID for direct updates — no GitHub comment list fetch.
	if len(commentEntries) > 0 {
		for _, comment := range giteaComments {
			if comment == nil {
				continue
			}
			if err := ctx.Err(); err != nil {
				return nil, fmt.Errorf("migrating comments: %v", err)
			}

			commentBody, giteaHash, githubHash := e.buildCommentBody(comment, giteaItemId, githubItemId)

			if cached, ok := commentEntries[comment.ID]; ok {
				if cached.GiteaHash == giteaHash && cached.GitHubHash == githubHash {
					e.Logger.Trace("existing comment is up-to-date (hash match)", "gitea_item", giteaItemId, "github_item", githubItemId, "comment_id", comment.ID)
					continue
				}
				e.Logger.Debug("updating comment (hash mismatch)", "gitea_item", giteaItemId, "github_item", githubItemId, "comment_id", comment.ID)
				if _, _, err := e.githubClient.Issues.EditComment(ctx, e.GitHubOwner, e.GitHubRepo, cached.GitHubCommentID, &github.IssueComment{Body: &commentBody}); err != nil {
					return nil, fmt.Errorf("updating comment: %v", err)
				}
				if err := h.SleepWithContext(ctx, constants.GithubApiPauseBetweenMutativeRequests); err != nil {
					return nil, err
				}
				commentEntries[comment.ID] = CommentCacheEntry{GiteaHash: giteaHash, GitHubHash: githubHash, GitHubCommentID: cached.GitHubCommentID}

				continue
			}

			e.Logger.Debug("creating comment", "gitea_item", giteaItemId, "github_item", githubItemId, "comment_id", comment.ID)
			created, _, err := e.githubClient.Issues.CreateComment(ctx, e.GitHubOwner, e.GitHubRepo, int(githubItemId), &github.IssueComment{Body: &commentBody})
			if err != nil {
				return nil, fmt.Errorf("creating comment for gitea item #%d (#%d): %v", giteaItemId, githubItemId, err)
			}
			if created.GetID() == 0 {
				return nil, fmt.Errorf("creating comment for gitea item #%d (#%d): GitHub returned ID 0", giteaItemId, githubItemId)
			}
			if err := h.SleepWithContext(ctx, constants.GithubApiPauseBetweenMutativeRequests); err != nil {
				return nil, err
			}
			commentEntries[comment.ID] = CommentCacheEntry{GiteaHash: giteaHash, GitHubHash: githubHash, GitHubCommentID: created.GetID()}
		}
		return commentEntries, nil
	}

	// non-cache-file mode / first-run: fetch GitHub comments and match by body content.
	e.Logger.Debug("retrieving GitHub comments", "gitea_item", giteaItemId, "github_item", githubItemId)
	githubComments, _, err := e.githubClient.Issues.ListComments(ctx, e.GitHubOwner, e.GitHubRepo, int(githubItemId), &github.IssueListCommentsOptions{Sort: h.Pointer("created"), Direction: h.Pointer("asc")})
	if err != nil {
		return nil, fmt.Errorf("listing github comments: %v", err)
	}

	for _, comment := range giteaComments {
		if comment == nil {
			continue
		}

		// Check for context cancellation
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("migrating comments: %v", err)
		}

		commentBody, giteaHash, githubHash := e.buildCommentBody(comment, giteaItemId, githubItemId)

		foundExistingComment := false
		for _, githubComment := range githubComments {
			if githubComment == nil {
				continue
			}

			if strings.Contains(githubComment.GetBody(), fmt.Sprintf("**Comment ID** | %d", comment.ID)) {
				foundExistingComment = true

				if githubComment.Body == nil || *githubComment.Body != commentBody {
					e.Logger.Debug("updating comment", "gitea_item", giteaItemId, "github_item", githubItemId, "comment_id", comment.ID, "comment_id", githubComment.GetID())
					githubComment.Body = &commentBody
					if _, _, err = e.githubClient.Issues.EditComment(ctx, e.GitHubOwner, e.GitHubRepo, githubComment.GetID(), githubComment); err != nil {
						// TODO: think about whether to allow "!foundExistingComment" branch to create a new comment on error; previously loop-break instead of return
						return nil, fmt.Errorf("updating comments: %v", err)
					}
					if err = h.SleepWithContext(ctx, constants.GithubApiPauseBetweenMutativeRequests); err != nil {
						return nil, err
					}
				} else {
					e.Logger.Trace("existing comment is up-to-date", "gitea_item", giteaItemId, "github_item", githubItemId, "comment_id", comment.ID, "comment_id", githubComment.GetID())
				}
				if githubComment.GetID() == 0 {
					return nil, fmt.Errorf("comment %d on gitea item #%d: GitHub returned ID 0", comment.ID, giteaItemId)
				}
				commentEntries[comment.ID] = CommentCacheEntry{GiteaHash: giteaHash, GitHubHash: githubHash, GitHubCommentID: githubComment.GetID()}
				break
			}
		}

		if !foundExistingComment {
			e.Logger.Debug("creating comment", "gitea_item", giteaItemId, "github_item", githubItemId, "comment_id", comment.ID)
			created, _, err := e.githubClient.Issues.CreateComment(ctx, e.GitHubOwner, e.GitHubRepo, int(githubItemId), &github.IssueComment{Body: &commentBody})
			if err != nil {
				return nil, fmt.Errorf("creating comment for gitea item #%d (#%d): %v", giteaItemId, githubItemId, err)
			}
			if created.GetID() == 0 {
				return nil, fmt.Errorf("creating comment for gitea item #%d (#%d): GitHub returned ID 0", giteaItemId, githubItemId)
			}
			if err = h.SleepWithContext(ctx, constants.GithubApiPauseBetweenMutativeRequests); err != nil {
				return nil, err
			}
			commentEntries[comment.ID] = CommentCacheEntry{GiteaHash: giteaHash, GitHubHash: githubHash, GitHubCommentID: created.GetID()}
		}
	}

	return commentEntries, nil
}
