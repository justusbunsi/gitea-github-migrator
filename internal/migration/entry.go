package migration

import (
	"fmt"

	"code.gitea.io/sdk/gitea"
	"github.com/go-git/go-git/v5"
	"github.com/google/go-github/v74/github"
	"github.com/hashicorp/go-hclog"
	h "github.com/justusbunsi/gitea-github-migrator/internal/helpers"
)

type Entry struct {
	GiteaOwner        string
	GiteaRepo         string
	GitHubOwner       string
	GitHubRepo        string
	Logger            hclog.Logger
	PRSuccessCount    int
	PRFailureCount    int
	IssueSuccessCount int
	IssueFailureCount int
	GitHubItemID      int64
	GiteaRepository   *gitea.Repository
	GitRepo           *git.Repository
	giteaClient       *gitea.Client
	githubClient      *github.Client
}

func NewEntry(giteaSlug, githubSlug string, giteaClient *gitea.Client, githubClient *github.Client, logger hclog.Logger) (*Entry, error) {
	giteaOwner, giteaRepo, err := h.ParseProjectSlug(giteaSlug)
	if err != nil {
		return nil, fmt.Errorf("invalid gitea project: %w", err)
	}
	githubOwner, githubRepo, err := h.ParseProjectSlug(githubSlug)
	if err != nil {
		return nil, fmt.Errorf("invalid github project: %w", err)
	}

	e := &Entry{
		GiteaOwner:   giteaOwner,
		GiteaRepo:    giteaRepo,
		GitHubOwner:  githubOwner,
		GitHubRepo:   githubRepo,
		Logger:       logger.With("GITEA", giteaSlug, "GITHUB", githubSlug),
		giteaClient:  giteaClient,
		githubClient: githubClient,
	}

	e.Logger.Info("searching for Gitea repository")
	e.GiteaRepository, _, err = e.giteaClient.GetRepo(e.GiteaOwner, e.GiteaRepo)
	if err != nil {
		return nil, fmt.Errorf("retrieve gitea repository %s: %v", giteaSlug, err)
	}

	if e.GiteaRepository == nil {
		return nil, fmt.Errorf("no matching Gitea repository found: %s", giteaSlug)
	}

	return e, nil
}

func (e *Entry) GetCacheID() string {
	return fmt.Sprintf("%s/%s,%s/%s", e.GiteaOwner, e.GiteaRepo, e.GitHubOwner, e.GitHubRepo)
}
