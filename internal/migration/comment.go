package migration

import (
	"context"
	"fmt"
	"strings"

	"code.gitea.io/sdk/gitea"
	"github.com/google/go-github/v74/github"
	"github.com/justusbunsi/gitea-github-migrator/internal/constants"
	h "github.com/justusbunsi/gitea-github-migrator/internal/helpers"
)

func (e *Entry) MigrateComments(ctx context.Context, giteaItemId int64, githubItemId int) error {
	var giteaComments []*gitea.Comment
	opts := &gitea.ListIssueCommentOptions{}

	e.Logger.Debug("retrieving Gitea comments", "item_id", giteaItemId)
	for {
		// Check for context cancellation
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("retrieving gitea comments: %v", err)
		}

		result, resp, err := e.giteaClient.ListIssueComments(e.GiteaOwner, e.GiteaRepo, giteaItemId, gitea.ListIssueCommentOptions{})
		if err != nil {
			return fmt.Errorf("listing gitea comments: %v", err)
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
		return nil
	}

	if githubItemId == 0 {
		return fmt.Errorf("GitHub item id is 0 and would cause the API to retrieve all comments across the repository - leading to high rate limit burning")
	}

	e.Logger.Debug("retrieving GitHub comments", "item_id", githubItemId)
	githubComments, _, err := e.githubClient.Issues.ListComments(ctx, e.GitHubOwner, e.GitHubRepo, githubItemId, &github.IssueListCommentsOptions{Sort: h.Pointer("created"), Direction: h.Pointer("asc")})
	if err != nil {
		return fmt.Errorf("listing github comments: %v", err)
	}

	for _, comment := range giteaComments {
		if comment == nil {
			continue
		}

		// Check for context cancellation
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("migrating comments: %v", err)
		}

		commentBody := fmt.Sprintf(`> [!NOTE]
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
		if len(commentBody) > constants.GithubBodyLimit {
			e.Logger.Warn("comment was truncated due to platform limits", "gitea_item", giteaItemId, "github_item", githubItemId, "comment_id", comment.ID)
			commentBody = strings.ReplaceAll(commentBody, "This comment was migrated from Gitea", "This comment was migrated from Gitea **and was truncated due to platform limits**")
			commentBody = commentBody[:constants.GithubBodyLimit] + "..."
		}

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
						return fmt.Errorf("updating comments: %v", err)
					}
					if err = h.SleepWithContext(ctx, constants.GithubApiPauseBetweenMutativeRequests); err != nil {
						return err
					}
				}
			} else {
				e.Logger.Trace("existing comment is up-to-date", "gitea_item", giteaItemId, "github_item", githubItemId, "comment_id", comment.ID, "comment_id", githubComment.GetID())
			}
		}

		if !foundExistingComment {
			e.Logger.Debug("creating comment", "gitea_item", giteaItemId, "github_item", githubItemId, "comment_id", comment.ID)
			newComment := github.IssueComment{
				Body: &commentBody,
			}
			if _, _, err = e.githubClient.Issues.CreateComment(ctx, e.GitHubOwner, e.GitHubRepo, githubItemId, &newComment); err != nil {
				return fmt.Errorf("creating comment for gitea item #%d (#%d): %v", giteaItemId, githubItemId, err)
			}
			if err = h.SleepWithContext(ctx, constants.GithubApiPauseBetweenMutativeRequests); err != nil {
				return err
			}
		}
	}

	return nil
}
