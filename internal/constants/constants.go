package constants

import "time"

const (
	DateFormat          = "Mon, 2 Jan 2006"
	DefaultGithubDomain = "github.com"
	DefaultGiteaDomain  = "gitea.com"
	GithubBodyLimit     = 58000
	// GithubApiPauseBetweenMutativeRequests https://docs.github.com/en/enterprise-cloud@latest/rest/using-the-rest-api/best-practices-for-using-the-rest-api?apiVersion=2026-03-10#pause-between-mutative-requests
	GithubApiPauseBetweenMutativeRequests = 1 * time.Second
	PhantomItemPoster                     = "gitea-github-migrator"
	PhantomItemTitle                      = "Gitea GitHub Migrator placeholder item"
	PhantomItemBody                       = "This issue was automatically created by the Gitea GitHub Migrator to preserve issue and pull request ordering."
)
