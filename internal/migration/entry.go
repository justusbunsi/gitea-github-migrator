package migration

import (
	"fmt"

	"code.gitea.io/sdk/gitea"
	"github.com/hashicorp/go-hclog"
	h "github.com/justusbunsi/gitea-github-migrator/internal/helpers"
)

type Entry struct {
	GiteaOwner  string
	GiteaRepo   string
	GitHubOwner string
	GitHubRepo  string
	Logger      hclog.Logger
	giteaClient *gitea.Client
}

func NewEntry(giteaSlug, githubSlug string, giteaClient *gitea.Client, logger hclog.Logger) (*Entry, error) {
	giteaOwner, giteaRepo, err := h.ParseProjectSlug(giteaSlug)
	if err != nil {
		return nil, fmt.Errorf("invalid gitea project: %w", err)
	}
	githubOwner, githubRepo, err := h.ParseProjectSlug(githubSlug)
	if err != nil {
		return nil, fmt.Errorf("invalid github project: %w", err)
	}

	return &Entry{
		GiteaOwner:  giteaOwner,
		GiteaRepo:   giteaRepo,
		GitHubOwner: githubOwner,
		GitHubRepo:  githubRepo,
		Logger:      logger.With("GITEA", giteaSlug, "GITHUB", githubSlug),
		giteaClient: giteaClient,
	}, nil
}
