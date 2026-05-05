package migration

import (
	"context"
	"fmt"

	"code.gitea.io/sdk/gitea"
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
