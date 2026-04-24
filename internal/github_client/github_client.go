package github_client

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/gofri/go-github-pagination/githubpagination"
	"github.com/google/go-github/v74/github"
	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/go-retryablehttp"
	"github.com/jferrl/go-githubauth"
	"golang.org/x/oauth2"

	"github.com/justusbunsi/gitea-github-migrator/internal/constants"
	"github.com/justusbunsi/gitea-github-migrator/internal/retry_client"
)

type Config struct {
	Domain            string
	Token             string
	AppID             string
	AppInstallationID int64
	AppPrivateKey     []byte
}

type searchModder struct {
	base http.RoundTripper
}

func (s *searchModder) RoundTrip(req *http.Request) (*http.Response, error) {
	if req != nil && req.URL != nil {
		if req.URL.Path == "/search/issues" {
			values := req.URL.Query()
			values.Set("advanced_search", "true")
			req.URL.RawQuery = values.Encode()
		}
	}
	return s.base.RoundTrip(req)
}

// retryingTokenSource wraps an oauth2.TokenSource and retries transient Token()
// failures with exponential backoff before giving up.
type retryingTokenSource struct {
	base   oauth2.TokenSource
	logger hclog.Logger
	ctx    context.Context
}

func (r *retryingTokenSource) Token() (*oauth2.Token, error) {
	backoff := 2 * time.Second
	var err error
	for attempt := range 3 {
		var t *oauth2.Token
		if t, err = r.base.Token(); err == nil {
			return t, nil
		}
		if attempt < 2 {
			r.logger.Warn("token renewal failed, retrying", "attempt", attempt+1, "error", err)
			select {
			case <-r.ctx.Done():
				return nil, r.ctx.Err()
			case <-time.After(backoff):
			}
			backoff *= 2
		}
	}
	return nil, err
}

// New creates a GitHub API client and returns a token getter for git operations.
// The token getter always returns a valid, possibly refreshed bearer token so it
// is safe to call immediately before a git push rather than caching the result.
func New(ctx context.Context, cfg Config, logger hclog.Logger) (*github.Client, func() (string, error), error) {
	retryClient := retry_client.New(logger)

	var tokenFunc func() (string, error)

	if cfg.AppID != "" {
		appTokenSource, err := githubauth.NewApplicationTokenSource(cfg.AppID, cfg.AppPrivateKey)
		if err != nil {
			return nil, nil, fmt.Errorf("creating GitHub App token source: %w", err)
		}
		opts := []githubauth.InstallationTokenSourceOpt{
			githubauth.WithHTTPClient(retry_client.New(logger).StandardClient()),
		}
		if cfg.Domain != constants.DefaultGithubDomain {
			opts = append(opts, githubauth.WithEnterpriseURL(fmt.Sprintf("https://%s", cfg.Domain)))
		}
		tokenSource := githubauth.NewInstallationTokenSource(cfg.AppInstallationID, appTokenSource, opts...)
		wrapped := &retryingTokenSource{base: tokenSource, logger: logger, ctx: ctx}
		// Inject oauth2.Transport inside the retry loop so each retry attempt gets a
		// fresh token — critical when a rate-limit cooldown outlasts the token lifetime.
		retryClient.HTTPClient.Transport = &oauth2.Transport{
			Source: wrapped,
			Base:   retryClient.HTTPClient.Transport,
		}
		tokenFunc = func() (string, error) {
			t, err := wrapped.Token()
			if err != nil {
				return "", err
			}
			return t.AccessToken, nil
		}
	} else {
		tokenFunc = func() (string, error) { return cfg.Token, nil }
	}

	transport := &searchModder{
		base: &retryablehttp.RoundTripper{Client: retryClient},
	}
	client := githubpagination.NewClient(transport, githubpagination.WithPerPage(100))

	var gh *github.Client
	if cfg.Domain == constants.DefaultGithubDomain {
		gh = github.NewClient(client)
	} else {
		githubUrl := fmt.Sprintf("https://%s", cfg.Domain)
		var err error
		if gh, err = github.NewClient(client).WithEnterpriseURLs(githubUrl, githubUrl); err != nil {
			return nil, nil, fmt.Errorf("configuring GitHub Enterprise URLs: %w", err)
		}
	}

	if cfg.Token != "" {
		gh = gh.WithAuthToken(cfg.Token)
	}

	return gh, tokenFunc, nil
}
