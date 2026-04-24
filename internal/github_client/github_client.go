package github_client

import (
	"fmt"
	"net/http"

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

// New creates a GitHub API client and returns a token getter for git operations.
// The token getter always returns a valid, possibly refreshed bearer token so it
// is safe to call immediately before a git push rather than caching the result.
func New(cfg Config, logger hclog.Logger) (*github.Client, func() (string, error), error) {
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
		// Inject oauth2.Transport inside the retry loop so each retry attempt gets a
		// fresh token — critical when a rate-limit cooldown outlasts the token lifetime.
		retryClient.HTTPClient.Transport = &oauth2.Transport{
			Source: tokenSource,
			Base:   retryClient.HTTPClient.Transport,
		}
		tokenFunc = func() (string, error) {
			t, err := tokenSource.Token()
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
