package main

import (
	"bytes"
	"context"
	"encoding/csv"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"code.gitea.io/sdk/gitea"
	"github.com/go-git/go-billy/v5/memfs"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/storage/memory"
	"github.com/gofri/go-github-pagination/githubpagination"
	"github.com/google/go-github/v74/github"
	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/go-retryablehttp"
	"github.com/justusbunsi/gitea-github-migrator/internal/constants"
	h "github.com/justusbunsi/gitea-github-migrator/internal/helpers"
	"github.com/justusbunsi/gitea-github-migrator/internal/migration"
	"github.com/justusbunsi/gitea-github-migrator/internal/retry_client"
)

var loop, report bool
var deleteExistingRepos, enablePullRequests, enableIssues, renameMasterToMain bool
var githubDomain, githubRepo, githubToken, githubUser, giteaDomain, giteaProject, giteaToken, projectsCsvPath, renameTrunkBranch string

var (
	cache          *objectCache
	errCount       int
	logger         hclog.Logger
	gh             *github.Client
	gi             *gitea.Client
	maxConcurrency int
)

type Project = []string

type Report struct {
	OwnerName         string
	RepoName          string
	IssuesCount       int
	PullRequestsCount int
}

func sendErr(err error) {
	errCount++
	logger.Error(err.Error())
}

func main() {
	var err error

	// Bypass pre-emptive rate limit checks in the GitHub client, as we will handle these via go-retryablehttp
	valueCtx := context.WithValue(context.Background(), github.BypassRateLimitCheck, true)

	// Assign a Done channel so we can abort on Ctrl-c
	ctx, cancel := context.WithCancel(valueCtx)

	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt)
	defer func() {
		signal.Stop(c)
		cancel()
	}()
	go func() {
		select {
		case <-c:
			cancel()
		case <-ctx.Done():
		}
	}()

	logger = hclog.New(&hclog.LoggerOptions{
		Name:  "gitea-github-migrator",
		Level: hclog.LevelFromString(os.Getenv("LOG_LEVEL")),
	})

	cache = newObjectCache()

	githubToken = os.Getenv("GITHUB_TOKEN")
	if githubToken == "" {
		logger.Error("missing environment variable", "name", "GITHUB_TOKEN")
		os.Exit(1)
	}

	giteaToken = os.Getenv("GITEA_TOKEN")
	if giteaToken == "" {
		logger.Error("missing environment variable", "name", "GITEA_TOKEN")
		os.Exit(1)
	}

	flag.BoolVar(&loop, "loop", false, "continue migrating until canceled")
	flag.BoolVar(&report, "report", false, "report on primitives to be migrated instead of beginning migration")

	flag.BoolVar(&deleteExistingRepos, "delete-existing-repos", false, "whether existing repositories should be deleted before migrating")
	flag.BoolVar(&enablePullRequests, "migrate-pull-requests", false, "whether pull requests should be migrated")
	flag.BoolVar(&enableIssues, "migrate-issues", false, "whether issues should be migrated")
	flag.BoolVar(&renameMasterToMain, "rename-master-to-main", false, "rename master branch to main and update pull requests (incompatible with -rename-trunk-branch)")

	flag.StringVar(&githubDomain, "github-domain", constants.DefaultGithubDomain, "specifies the GitHub domain to use")
	flag.StringVar(&githubRepo, "github-repo", "", "the GitHub repository to migrate to")
	flag.StringVar(&githubUser, "github-user", "", "specifies the GitHub user to use, who will author any migrated PRs (required)")
	flag.StringVar(&giteaDomain, "gitea-domain", constants.DefaultGiteaDomain, "specifies the Gitea domain to use")
	flag.StringVar(&giteaProject, "gitea-project", "", "the Gitea project to migrate")
	flag.StringVar(&projectsCsvPath, "projects-csv", "", "specifies the path to a CSV file describing projects to migrate (incompatible with -gitea-project and -github-repo)")
	flag.StringVar(&renameTrunkBranch, "rename-trunk-branch", "", "specifies the new trunk branch name (incompatible with -rename-master-to-main)")

	flag.IntVar(&maxConcurrency, "max-concurrency", 4, "how many projects to migrate in parallel")

	flag.Parse()

	if githubUser == "" {
		githubUser = os.Getenv("GITHUB_USER")
	}

	if githubUser == "" {
		logger.Error("must specify GitHub user")
		os.Exit(1)
	}

	repoSpecifiedInline := githubRepo != "" && giteaProject != ""
	if repoSpecifiedInline && projectsCsvPath != "" {
		logger.Error("cannot specify -projects-csv and either -github-repo or -gitea-project at the same time")
		os.Exit(1)
	}
	if !repoSpecifiedInline && projectsCsvPath == "" {
		logger.Error("must specify either -projects-csv or both of -github-repo and -gitea-project")
		os.Exit(1)
	}

	if renameMasterToMain && renameTrunkBranch != "" {
		logger.Error("cannot specify -rename-master-to-main and -rename-trunk-branch together")
		os.Exit(1)
	}

	transport := &gitHubAdvancedSearchModder{
		base: &retryablehttp.RoundTripper{Client: retry_client.New(logger)},
	}
	client := githubpagination.NewClient(transport, githubpagination.WithPerPage(100))

	if githubDomain == constants.DefaultGithubDomain {
		gh = github.NewClient(client).WithAuthToken(githubToken)
	} else {
		githubUrl := fmt.Sprintf("https://%s", githubDomain)
		if gh, err = github.NewClient(client).WithAuthToken(githubToken).WithEnterpriseURLs(githubUrl, githubUrl); err != nil {
			sendErr(err)
			os.Exit(1)
		}
	}

	giteaUrl := fmt.Sprintf("https://%s", giteaDomain)
	if gi, err = gitea.NewClient(giteaUrl, gitea.SetToken(giteaToken)); err != nil {
		sendErr(err)
		os.Exit(1)
	}

	projects := make([]Project, 0)
	if projectsCsvPath != "" {
		data, err := os.ReadFile(projectsCsvPath)
		if err != nil {
			sendErr(err)
			os.Exit(1)
		}

		// Trim a UTF-8 BOM, if present
		data = bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))

		if projects, err = csv.NewReader(bytes.NewBuffer(data)).ReadAll(); err != nil {
			sendErr(err)
			os.Exit(1)
		}
	} else {
		projects = []Project{{giteaProject, githubRepo}}
	}

	if report {
		printReport(ctx, projects)
	} else {
		if err = performMigration(ctx, projects); err != nil {
			sendErr(err)
			os.Exit(1)
		} else if errCount > 0 {
			logger.Warn(fmt.Sprintf("encountered %d errors during migration, review log output for details", errCount))
			os.Exit(1)
		}
	}
}

func printReport(ctx context.Context, projects []Project) {
	logger.Debug("building report")

	results := make([]Report, 0)

	for _, proj := range projects {
		if err := ctx.Err(); err != nil {
			return
		}

		result, err := reportProject(ctx, proj)
		if err != nil {
			sendErr(err)
		}

		if result != nil {
			results = append(results, *result)
		}
	}

	fmt.Println()

	totalIssues := 0
	totalPullRequests := 0
	for _, result := range results {
		totalIssues += result.IssuesCount
		totalPullRequests += result.PullRequestsCount
		fmt.Printf("%#v\n", result)
	}

	fmt.Println()
	fmt.Printf("Total issues: %d\n", totalIssues)
	fmt.Printf("Total pull requests: %d\n", totalPullRequests)
	fmt.Println()
}

func reportProject(ctx context.Context, proj []string) (*Report, error) {
	entry, err := migration.NewEntry(proj[0], proj[1], gi, gh, logger)
	if err != nil {
		return nil, err
	}

	_, issueCount, err := entry.GetAllGiteaIssues(ctx, gitea.IssueTypeIssue, true)
	if err != nil {
		return nil, err
	}

	_, prCount, err := entry.GetAllGiteaIssues(ctx, gitea.IssueTypePull, true)
	if err != nil {
		return nil, err
	}

	return &Report{
		OwnerName:         entry.GiteaOwner,
		RepoName:          entry.GiteaRepo,
		IssuesCount:       issueCount,
		PullRequestsCount: prCount,
	}, nil
}

func performMigration(ctx context.Context, projects []Project) error {
	concurrency := maxConcurrency
	if len(projects) < maxConcurrency {
		concurrency = len(projects)
	}

	logger.Info(fmt.Sprintf("processing %d project(s) with %d workers", len(projects), concurrency))

	var wg sync.WaitGroup
	queue := make(chan Project, concurrency*2)

	for i := 0; i < concurrency; i++ {
		wg.Add(1)

		go func() {
			defer wg.Done()

			for proj := range queue {
				if err := ctx.Err(); err != nil {
					break
				}

				if err := migrateProject(ctx, proj); err != nil {
					sendErr(err)
				}
			}
		}()
	}

	queueProjects := func() {
		for _, proj := range projects {
			if err := ctx.Err(); err != nil {
				break
			}

			queue <- proj
		}
	}

	if loop {
		logger.Info(fmt.Sprintf("looping migration until canceled"))
		for {
			if err := ctx.Err(); err != nil {
				break
			}

			queueProjects()
		}
	} else {
		queueProjects()
		close(queue)
	}

	wg.Wait()

	return nil
}

func migrateProject(ctx context.Context, proj []string) error {
	entry, err := migration.NewEntry(proj[0], proj[1], gi, gh, logger)
	if err != nil {
		return err
	}

	cloneUrl, err := url.Parse(entry.GiteaRepository.CloneURL)
	if err != nil {
		return fmt.Errorf("parsing clone URL: %v", err)
	}

	entry.Logger.Info("mirroring repository from Gitea to GitHub")

	user, err := getGithubUser(ctx, entry.GitHubOwner)
	if err != nil {
		return fmt.Errorf("retrieving github user: %v", err)
	}

	var org string
	if strings.EqualFold(*user.Type, "organization") {
		org = entry.GitHubOwner
	} else if !strings.EqualFold(*user.Type, "user") || !strings.EqualFold(*user.Login, entry.GitHubOwner) {
		return fmt.Errorf("configured owner is neither an organization nor the current user: %s", entry.GitHubOwner)
	}

	entry.Logger.Debug("checking for existing repository on GitHub")
	_, _, err = gh.Repositories.Get(ctx, entry.GitHubOwner, entry.GitHubRepo)

	var githubError *github.ErrorResponse
	if err != nil && (!errors.As(err, &githubError) || githubError == nil || githubError.Response == nil || githubError.Response.StatusCode != http.StatusNotFound) {
		return fmt.Errorf("retrieving github repo: %v", err)
	}

	var createRepo, repoDeleted bool
	if err != nil {
		createRepo = true
	} else if deleteExistingRepos {
		entry.Logger.Warn("existing repository was found on GitHub, proceeding to delete")
		if _, err = gh.Repositories.Delete(ctx, entry.GitHubOwner, entry.GitHubRepo); err != nil {
			return fmt.Errorf("deleting existing github repo: %v", err)
		}

		createRepo = true
		repoDeleted = true
	}

	defaultBranch := "main"
	if renameTrunkBranch != "" {
		defaultBranch = renameTrunkBranch
	} else if !renameMasterToMain && entry.GiteaRepository.DefaultBranch != "" {
		defaultBranch = entry.GiteaRepository.DefaultBranch
	}

	homepage := fmt.Sprintf("https://%s/%s/%s", giteaDomain, entry.GiteaOwner, entry.GiteaRepo)
	description := regexp.MustCompile("\r|\n").ReplaceAllString(entry.GiteaRepository.Description, " ")

	if createRepo {
		if repoDeleted {
			entry.Logger.Warn("recreating GitHub repository")
		} else {
			entry.Logger.Debug("repository not found on GitHub, proceeding to create")
		}
		newRepo := github.Repository{
			Name:          h.Pointer(entry.GitHubRepo),
			Description:   &description,
			Homepage:      &homepage,
			DefaultBranch: &defaultBranch,
			Private:       h.Pointer(true),
			HasIssues:     h.Pointer(true),
			HasProjects:   h.Pointer(true),
			HasWiki:       h.Pointer(true),
		}
		if _, _, err = gh.Repositories.Create(ctx, org, &newRepo); err != nil {
			return fmt.Errorf("creating github repo: %v", err)
		}
	}

	entry.Logger.Debug("updating repository settings")
	updateRepo := github.Repository{
		Name:              h.Pointer(entry.GitHubRepo),
		Description:       &description,
		Homepage:          &homepage,
		AllowAutoMerge:    h.Pointer(true),
		AllowMergeCommit:  h.Pointer(true),
		AllowRebaseMerge:  h.Pointer(true),
		AllowSquashMerge:  h.Pointer(true),
		AllowUpdateBranch: h.Pointer(true),
	}
	if _, _, err = gh.Repositories.Edit(ctx, entry.GitHubOwner, entry.GitHubRepo, &updateRepo); err != nil {
		return fmt.Errorf("updating github repo: %v", err)
	}

	cloneUrl.User = url.UserPassword("oauth2", giteaToken)
	cloneUrlWithCredentials := cloneUrl.String()

	// In-memory filesystem for worktree operations
	fs := memfs.New()

	entry.Logger.Debug("cloning repository", "url", entry.GiteaRepository.CloneURL)
	entry.GitRepo, err = git.CloneContext(ctx, memory.NewStorage(), fs, &git.CloneOptions{
		URL:        cloneUrlWithCredentials,
		Auth:       nil,
		RemoteName: "gitea",
		Mirror:     true,
	})
	if err != nil {
		return fmt.Errorf("cloning gitea repo: %v", err)
	}

	if defaultBranch != entry.GiteaRepository.DefaultBranch {
		if giteaTrunk, err := entry.GitRepo.Reference(plumbing.NewBranchReferenceName(entry.GiteaRepository.DefaultBranch), false); err == nil {
			entry.Logger.Info("renaming trunk branch prior to push", "gitea_trunk", entry.GiteaRepository.DefaultBranch, "github_trunk", defaultBranch, "sha", giteaTrunk.Hash())

			entry.Logger.Debug("creating new trunk branch", "github_trunk", defaultBranch, "sha", giteaTrunk.Hash())
			githubTrunk := plumbing.NewHashReference(plumbing.NewBranchReferenceName(defaultBranch), giteaTrunk.Hash())
			if err = entry.GitRepo.Storer.SetReference(githubTrunk); err != nil {
				return fmt.Errorf("creating trunk branch: %v", err)
			}

			entry.Logger.Debug("deleting old trunk branch", "gitea_trunk", entry.GiteaRepository.DefaultBranch, "sha", giteaTrunk.Hash())
			if err = entry.GitRepo.Storer.RemoveReference(giteaTrunk.Name()); err != nil {
				return fmt.Errorf("deleting old trunk branch: %v", err)
			}
		}
	}

	githubUrl := fmt.Sprintf("https://%s/%s/%s", githubDomain, entry.GitHubOwner, entry.GitHubRepo)
	githubUrlWithCredentials := fmt.Sprintf("https://%s:%s@%s/%s/%s", githubUser, githubToken, githubDomain, entry.GitHubOwner, entry.GitHubRepo)

	entry.Logger.Debug("adding remote for GitHub repository", "url", githubUrl)
	if _, err = entry.GitRepo.CreateRemote(&config.RemoteConfig{
		Name:   "github",
		URLs:   []string{githubUrlWithCredentials},
		Mirror: true,
	}); err != nil {
		return fmt.Errorf("adding github remote: %v", err)
	}

	entry.Logger.Debug("force-pushing to GitHub repository", "url", githubUrl)
	if err = entry.GitRepo.PushContext(ctx, &git.PushOptions{
		RemoteName: "github",
		Force:      true,
		//Prune:      true, // causes error, attempts to delete main branch
	}); err != nil {
		if errors.Is(err, git.NoErrAlreadyUpToDate) {
			entry.Logger.Debug("repository already up-to-date on GitHub", "url", githubUrl)
		} else {
			return fmt.Errorf("pushing to github repo: %v", err)
		}
	}

	entry.Logger.Debug("pushing tags to GitHub repository", "url", githubUrl)
	if err = entry.GitRepo.PushContext(ctx, &git.PushOptions{
		RemoteName: "github",
		Force:      true,
		RefSpecs:   []config.RefSpec{"refs/tags/*:refs/tags/*"},
	}); err != nil {
		if errors.Is(err, git.NoErrAlreadyUpToDate) {
			entry.Logger.Debug("repository already up-to-date on GitHub", "url", githubUrl)
		} else {
			return fmt.Errorf("pushing to github repo: %v", err)
		}
	}

	entry.Logger.Debug("setting default repository branch", "branch_name", defaultBranch)
	updateRepo = github.Repository{
		DefaultBranch: &defaultBranch,
	}
	if _, _, err = gh.Repositories.Edit(ctx, entry.GitHubOwner, entry.GitHubRepo, &updateRepo); err != nil {
		return fmt.Errorf("setting default branch: %v", err)
	}

	if enableIssues && enablePullRequests {
		giteaItems, totalCount, err := entry.GetAllGiteaIssues(ctx, gitea.IssueTypeAll, false)
		if err != nil {
			return err
		}

		giteaPullRequests, prTotalCount, err := entry.GetAllGiteaPullRequests(ctx, false)
		if err != nil {
			return err
		}

		// To preserve item IDs and their orders while migrating them, we need to detect ID gaps in the list.
		// Those items were initially deleted and allocated an ID, so we inject a phantom issue with the missing ID to prevent mismatches.
		phantomItemsCount := 0
		filled := make([]*gitea.Issue, 0, len(giteaItems))
		nextExpected := int64(1)
		for _, issue := range giteaItems {
			for ; nextExpected < issue.Index; nextExpected++ {
				filled = append(filled, &gitea.Issue{
					ID:    0,
					Index: nextExpected,
					Poster: &gitea.User{
						UserName: constants.PhantomItemPoster,
					},
					Title: constants.PhantomItemTitle,
					Body:  constants.PhantomItemBody,
				})
				phantomItemsCount++
			}
			filled = append(filled, issue)
			nextExpected = issue.Index + 1
		}
		giteaItems = filled

		entry.Logger.Info("migrating issues and pull requests from Gitea to GitHub", "issues", totalCount-prTotalCount, "pull_requests", prTotalCount, "phantom_items", phantomItemsCount)

		for _, giteaItem := range giteaItems {
			if giteaItem == nil {
				sendErr(fmt.Errorf("gitea item is nil - this should not exist"))
				// fail-fast
				break
			}

			// Check for context cancellation
			if err := ctx.Err(); err != nil {
				sendErr(fmt.Errorf("preparing to migrate item: %v", err))
				break
			}

			if giteaItem.PullRequest != nil {
				idx := slices.IndexFunc(giteaPullRequests, func(pr *gitea.PullRequest) bool {
					return pr.Index == giteaItem.Index
				})
				if idx == -1 {
					sendErr(fmt.Errorf("pull request #%d not found in dataset", giteaItem.Index))
					entry.PRFailureCount++
					// fail-fast
					break
				}
				err = migratePullRequest(ctx, entry, defaultBranch, createRepo, giteaPullRequests[idx])
				if err != nil {
					sendErr(err)
					entry.PRFailureCount++
					// fail-fast
					entry.Logger.Error("stop migration due to error to prevent ID mismatch")
					break
				}
			} else {
				err = migrateIssue(ctx, entry, createRepo, giteaItem)
				if err != nil {
					sendErr(err)
					entry.IssueFailureCount++
					// fail-fast
					entry.Logger.Error("stop migration due to error to prevent ID mismatch")
					break
				}
			}
		}

		issueSkipped := totalCount + phantomItemsCount - prTotalCount - entry.IssueSuccessCount - entry.IssueFailureCount
		prSkipped := prTotalCount - entry.PRSuccessCount - entry.PRFailureCount
		entry.Logger.Info("migrated issues from Gitea to GitHub", "successful", entry.IssueSuccessCount, "failed", entry.IssueFailureCount, "skipped", issueSkipped)
		entry.Logger.Info("migrated pull requests from Gitea to GitHub", "successful", entry.PRSuccessCount, "failed", entry.PRFailureCount, "skipped", prSkipped)
	} else {
		if enableIssues {
			giteaIssues, totalCount, err := entry.GetAllGiteaIssues(ctx, gitea.IssueTypeIssue, false)
			if err != nil {
				return err
			}

			entry.Logger.Info("migrating issues from Gitea to GitHub", "count", totalCount)
			for _, giteaIssue := range giteaIssues {
				if giteaIssue == nil {
					continue
				}

				if err := ctx.Err(); err != nil {
					sendErr(fmt.Errorf("preparing to migrate issue: %v", err))
					break
				}

				err = migrateIssue(ctx, entry, createRepo, giteaIssue)
				if err != nil {
					sendErr(err)
					entry.IssueFailureCount++
				}
			}

			skippedCount := totalCount - entry.IssueSuccessCount - entry.IssueFailureCount
			entry.Logger.Info("migrated issues from Gitea to GitHub", "successful", entry.IssueSuccessCount, "failed", entry.IssueFailureCount, "skipped", skippedCount)
		}

		if enablePullRequests {
			giteaPullRequests, totalCount, err := entry.GetAllGiteaPullRequests(ctx, false)
			if err != nil {
				return err
			}

			entry.Logger.Info("migrating pull requests from Gitea to GitHub", "count", totalCount)
			for _, giteaPullRequest := range giteaPullRequests {
				if giteaPullRequest == nil {
					continue
				}

				// Check for context cancellation
				if err := ctx.Err(); err != nil {
					sendErr(fmt.Errorf("preparing to list pull requests: %v", err))
					break
				}

				err = migratePullRequest(ctx, entry, defaultBranch, createRepo, giteaPullRequest)
				if err != nil {
					sendErr(err)
					entry.PRFailureCount++
				}
			}

			skippedCount := totalCount - entry.PRSuccessCount - entry.PRFailureCount
			entry.Logger.Info("migrated pull requests from Gitea to GitHub", "successful", entry.PRSuccessCount, "failed", entry.PRFailureCount, "skipped", skippedCount)
		}
	}

	return nil
}

func migratePullRequest(ctx context.Context, entry *migration.Entry, defaultBranch string, initialMigration bool, giteaPullRequest *gitea.PullRequest) error {
	if giteaPullRequest.MergeBase == "" {
		return fmt.Errorf("identifying suitable merge base for pull request %d", giteaPullRequest.Index)
	}

	sourceBranchForClosedPullRequest := fmt.Sprintf("migration-source-%d/%s", giteaPullRequest.Index, giteaPullRequest.Head.Ref)
	targetBranchForClosedPullRequest := fmt.Sprintf("migration-target-%d/%s", giteaPullRequest.Index, giteaPullRequest.Base.Ref)

	var cleanUpBranch, tmpEmptyCommitRequired bool
	var githubPullRequest *github.PullRequest
	entry.Logger.Trace("retrieve pull request head ref", "pr_number", giteaPullRequest.Index)
	prHeadRefs, _, err := gi.GetRepoRefs(entry.GiteaOwner, entry.GiteaRepo, fmt.Sprintf("pull/%d/head", giteaPullRequest.Index))
	if err != nil {
		return fmt.Errorf("retrieve head ref for pull request %d: %v", giteaPullRequest.Index, err)
	}
	if len(prHeadRefs) == 0 {
		return fmt.Errorf("no head ref for pull request %d found", giteaPullRequest.Index)
	}
	prHeadRef := prHeadRefs[0].Object.SHA

	// Some pull requests have no commits, flag these for later handling
	if strings.EqualFold(giteaPullRequest.MergeBase, prHeadRef) {
		entry.Logger.Debug("pull request with empty commit list", "pr_number", giteaPullRequest.Index)
		tmpEmptyCommitRequired = true
	}

	if !initialMigration {
		entry.Logger.Debug("searching for any existing pull request", "pr_number", giteaPullRequest.Index)
		sourceBranches := []string{giteaPullRequest.Head.Ref, sourceBranchForClosedPullRequest}
		branchQuery := fmt.Sprintf("head:%s", strings.Join(sourceBranches, " OR head:"))
		query := fmt.Sprintf("repo:%s/%s AND is:pr AND (%s)", entry.GitHubOwner, entry.GitHubRepo, branchQuery)
		searchResult, err := getGithubSearchResults(ctx, query)
		if err != nil {
			return fmt.Errorf("listing pull requests: %v", err)
		}

		// Look for an existing GitHub pull request
		skip := false
		for _, issue := range searchResult.Issues {
			if issue == nil {
				continue
			}

			// Check for context cancellation
			if err := ctx.Err(); err != nil {
				return fmt.Errorf("preparing to retrieve pull request: %v", err)
			}

			if issue.IsPullRequest() {
				// Extract the PR number from the URL
				prUrl, err := url.Parse(*issue.PullRequestLinks.URL)
				if err != nil {
					sendErr(fmt.Errorf("parsing pull request url: %v", err))
					skip = true
					break
				}

				if m := regexp.MustCompile(".+/([0-9]+)$").FindStringSubmatch(prUrl.Path); len(m) == 2 {
					prNumber, _ := strconv.Atoi(m[1])
					ghPr, err := getGithubPullRequest(ctx, entry.GitHubOwner, entry.GitHubRepo, prNumber)
					if err != nil {
						sendErr(fmt.Errorf("retrieving pull request: %v", err))
						skip = true
						break
					}

					if strings.Contains(ghPr.GetBody(), fmt.Sprintf("**Gitea PR Number** | [%d]", giteaPullRequest.Index)) {
						entry.Logger.Debug("found existing pull request", "pr_number", ghPr.GetNumber())
						githubPullRequest = ghPr
						break
					}
				}
			}
		}
		if skip {
			return nil
		}
	}

	worktree, err := entry.GitRepo.Worktree()
	if err != nil {
		return fmt.Errorf("creating worktree: %v", err)
	}

	if githubPullRequest == nil {
		entry.Logger.Trace("loading pull request head commit", "pr_number", giteaPullRequest.Index, "head_sha", prHeadRef)
		prHeadHash := plumbing.NewHash(prHeadRef)
		prHeadCommit, err := object.GetCommit(entry.GitRepo.Storer, prHeadHash)
		if err != nil {
			return fmt.Errorf("loading pull request head commit: %v", err)
		}
		entry.Logger.Trace("loading merge base", "pr_number", giteaPullRequest.Index, "pr_merge_base", giteaPullRequest.MergeBase)
		mergeBaseHash := plumbing.NewHash(giteaPullRequest.MergeBase)
		mergeBaseCommit, err := object.GetCommit(entry.GitRepo.Storer, mergeBaseHash)
		if err != nil {
			return fmt.Errorf("loading merge base: %v", err)
		}
		entry.Logger.Trace("detecting best common ancestor", "pr_number", giteaPullRequest.Index, "base", mergeBaseHash, "head", prHeadHash)
		bases, err := mergeBaseCommit.MergeBase(prHeadCommit)
		if err != nil {
			return fmt.Errorf("detecting best common ancestor: %v", err)
		}
		if len(bases) == 0 {
			entry.Logger.Trace("orphaned head commit detected", "pr_number", giteaPullRequest.Index, "sha", prHeadHash)
			tmpEmptyCommitRequired = true
		}

		if tmpEmptyCommitRequired {
			entry.Logger.Trace("checkout source branch for empty commit list pull request", "repository_id", entry.GiteaRepository.ID, "pr_number", giteaPullRequest.Index)
			if err = worktree.Checkout(&git.CheckoutOptions{
				Create: h.Conditional(giteaPullRequest.State == gitea.StateOpen, false, true),
				Force:  true,
				Branch: plumbing.NewBranchReferenceName(h.Conditional(giteaPullRequest.State == gitea.StateClosed, sourceBranchForClosedPullRequest, giteaPullRequest.Head.Ref)),
			}); err != nil {
				return fmt.Errorf("checking out temporary empty commit list source branch for pull request #%d: %v", giteaPullRequest.Index, err)
			}

			resetHash := h.Conditional(len(bases) == 0, mergeBaseHash, prHeadHash)
			entry.Logger.Trace("reset worktree for migrator-required empty commit", "pr_number", giteaPullRequest.Index, "reset_to_sha", resetHash)
			if err = worktree.Reset(&git.ResetOptions{Mode: git.HardReset, Commit: resetHash}); err != nil {
				return fmt.Errorf("reset empty commit list pull request branch: %v", err)
			}

			entry.Logger.Debug("creating empty migration commit", "pr_number", giteaPullRequest.Index)
			prHeadHash, err = worktree.Commit("Migrator-required empty commit", &git.CommitOptions{
				AllowEmptyCommits: true,
				Author: &object.Signature{
					Name:  "Gitea GitHub Migrator",
					Email: "gitea-github-migrator@example.com",
					When:  time.Now(),
				},
				Committer: &object.Signature{
					Name:  "Gitea GitHub Migrator",
					Email: "gitea-github-migrator@example.com",
					When:  time.Now(),
				},
			})
			if err != nil {
				return fmt.Errorf("creating empty migration commit: %v", err)
			}

			if strings.EqualFold(string(giteaPullRequest.State), string(gitea.StateOpen)) {
				entry.Logger.Trace("pushing source branch for empty commit list pull request", "repository_id", entry.GiteaRepository.ID, "pr_number", giteaPullRequest.Index)

				if err = entry.GitRepo.PushContext(ctx, &git.PushOptions{
					RemoteName: "github",
					RefSpecs: []config.RefSpec{
						config.RefSpec(fmt.Sprintf("refs/heads/%[1]s:refs/heads/%[1]s", giteaPullRequest.Head.Ref)),
					},
					Force: true,
				}); err != nil {
					if errors.Is(err, git.NoErrAlreadyUpToDate) {
						entry.Logger.Trace("empty commit list branch already exists and is up-to-date on GitHub", "source_branch", giteaPullRequest.Head.Ref, "target_branch", giteaPullRequest.Base.Ref)
					} else {
						return fmt.Errorf("pushing temporary empty commit list branch to github: %v", err)
					}
				}
			}
		}

		// Proceed to create temporary branches when migrating a merged/closed merge request that doesn't yet have a counterpart PR in GitHub (can't create one without a branch)
		if !strings.EqualFold(string(giteaPullRequest.State), string(gitea.StateOpen)) {
			entry.Logger.Trace("searching for existing branch for closed/merged pull request", "repository_id", entry.GiteaRepository.ID, "pr_number", giteaPullRequest.Index, "source_branch", giteaPullRequest.Head.Ref)

			// Generate temporary branch names
			giteaPullRequest.Head.Ref = sourceBranchForClosedPullRequest
			giteaPullRequest.Base.Ref = targetBranchForClosedPullRequest

			entry.Logger.Trace("creating target branch for merged/closed pull request", "repository_id", entry.GiteaRepository.ID, "pr_number", giteaPullRequest.Index, "branch", giteaPullRequest.Base.Ref, "sha", mergeBaseCommit.Hash)
			if err = worktree.Checkout(&git.CheckoutOptions{
				Create: true,
				Force:  true,
				Branch: plumbing.NewBranchReferenceName(giteaPullRequest.Base.Ref),
				Hash:   mergeBaseCommit.Hash,
			}); err != nil {
				return fmt.Errorf("checking out temporary target branch: %v", err)
			}

			entry.Logger.Trace("creating source branch for merged/closed pull request", "repository_id", entry.GiteaRepository.ID, "pr_number", giteaPullRequest.Index, "branch", giteaPullRequest.Head.Ref, "sha", prHeadHash)
			if err = worktree.Checkout(&git.CheckoutOptions{
				Create: h.Conditional(tmpEmptyCommitRequired, false, true),
				Force:  true,
				Branch: plumbing.NewBranchReferenceName(giteaPullRequest.Head.Ref),
				Hash:   h.Conditional(tmpEmptyCommitRequired, plumbing.ZeroHash, prHeadHash),
			}); err != nil {
				return fmt.Errorf("checking out temporary source branch: %v", err)
			}

			entry.Logger.Debug("pushing branches for merged/closed pull request", "source_branch", giteaPullRequest.Head.Ref, "target_branch", giteaPullRequest.Base.Ref)
			if err = entry.GitRepo.PushContext(ctx, &git.PushOptions{
				RemoteName: "github",
				RefSpecs: []config.RefSpec{
					config.RefSpec(fmt.Sprintf("refs/heads/%[1]s:refs/heads/%[1]s", giteaPullRequest.Head.Ref)),
					config.RefSpec(fmt.Sprintf("refs/heads/%[1]s:refs/heads/%[1]s", giteaPullRequest.Base.Ref)),
				},
				Force: true,
			}); err != nil {
				if errors.Is(err, git.NoErrAlreadyUpToDate) {
					entry.Logger.Trace("branch already exists and is up-to-date on GitHub", "source_branch", giteaPullRequest.Head.Ref, "target_branch", giteaPullRequest.Base.Ref)
				} else {
					return fmt.Errorf("pushing temporary branches to github: %v", err)
				}
			}

			// We will clean up these temporary branches after configuring and closing the pull request
			cleanUpBranch = true
		}
	}

	if defaultBranch != entry.GiteaRepository.DefaultBranch && giteaPullRequest.Base.Ref == entry.GiteaRepository.DefaultBranch {
		entry.Logger.Trace("changing target trunk branch", "repository_id", entry.GiteaRepository.ID, "pr_number", giteaPullRequest.Index, "old_trunk", entry.GiteaRepository.DefaultBranch, "new_trunk", defaultBranch)
		giteaPullRequest.Base.Ref = defaultBranch
	}

	originalState := ""
	if !strings.EqualFold(string(giteaPullRequest.State), string(gitea.StateOpen)) {
		if giteaPullRequest.HasMerged && giteaPullRequest.Merged != nil {
			originalState = fmt.Sprintf("> This pull request was originally **merged** on Gitea")
		} else {
			originalState = fmt.Sprintf("> This pull request was originally **closed** on Gitea")
		}
	}

	entry.Logger.Debug("determining pull request approvers", "repository_id", entry.GiteaRepository.ID, "pr_number", giteaPullRequest.Index)
	approvers := make([]string, 0)
	reviews, _, err := gi.ListPullReviews(entry.GiteaOwner, entry.GiteaRepo, giteaPullRequest.Index, gitea.ListPullReviewsOptions{})
	if err != nil {
		sendErr(fmt.Errorf("listing pull request reviews: %v", err))
	} else {
		for _, review := range reviews {
			if review.State == gitea.ReviewStateApproved {
				approvers = append(approvers, h.GetGitHubAccountReference(review.Reviewer))
			}
		}
	}

	description := giteaPullRequest.Body
	if strings.TrimSpace(description) == "" {
		description = "_No description_"
	}

	slices.Sort(approvers)
	approval := strings.Join(approvers, ", ")
	if approval == "" {
		approval = "_No approvers_"
	}

	closeDetails := ""
	if giteaPullRequest.State == gitea.StateClosed {
		if giteaPullRequest.HasMerged && giteaPullRequest.Merged != nil {
			closeDetails = fmt.Sprintf("\n> | **Date Originally Merged** | %s |\n> | **Original Merger** | %s |\n> | **Merge Commit** | %s |", giteaPullRequest.Merged.Format(constants.DateFormat), giteaPullRequest.MergedBy.UserName, *giteaPullRequest.MergedCommitID)
		} else if giteaPullRequest.Closed != nil {
			closeDetails = fmt.Sprintf("\n> | **Date Originally Closed** | %s |", giteaPullRequest.Closed.Format(constants.DateFormat))
		}
	}

	body := fmt.Sprintf(`> [!NOTE]
> This pull request was migrated from Gitea
>
> |      |      |
> | ---- | ---- |
> | **Original Author** | %[1]s |
> | **Gitea Repository** | [%[4]s/%[5]s](https://%[10]s/%[4]s/%[5]s) |
> | **Gitea Pull Request** | [%[11]s](https://%[10]s/%[4]s/%[5]s/pulls/%[2]d) |
> | **Gitea PR Number** | [%[2]d](https://%[10]s/%[4]s/%[5]s/pulls/%[2]d) |
> | **Date Originally Opened** | %[6]s |%[7]s
> | **Approved on Gitea by** | %[8]s |
> |      |      |
>
%[9]s

## Original Description

%[3]s`, h.GetGitHubAccountReference(giteaPullRequest.Poster), giteaPullRequest.Index, description, entry.GiteaOwner, entry.GiteaRepo, giteaPullRequest.Created.Format(constants.DateFormat), closeDetails, approval, originalState, giteaDomain, giteaPullRequest.Title)

	if len(body) > constants.GithubBodyLimit {
		entry.Logger.Warn("pull request body was truncated due to platform limits", "source_branch", giteaPullRequest.Head.Ref, "target_branch", giteaPullRequest.Base.Ref)
		body = h.SmartRenovateBodyTruncate(body)
	}

	if githubPullRequest == nil {
		entry.Logger.Info("creating pull request", "source_branch", giteaPullRequest.Head.Ref, "target_branch", giteaPullRequest.Base.Ref)
		newPullRequest := github.NewPullRequest{
			Title:               &giteaPullRequest.Title,
			Head:                &giteaPullRequest.Head.Ref,
			Base:                &giteaPullRequest.Base.Ref,
			Body:                &body,
			MaintainerCanModify: h.Pointer(true),
			Draft:               &giteaPullRequest.Draft,
		}
		if githubPullRequest, _, err = gh.PullRequests.Create(ctx, entry.GitHubOwner, entry.GitHubRepo, &newPullRequest); err != nil {
			return fmt.Errorf("creating pull request: %v", err)
		}
		time.Sleep(constants.GithubApiPauseBetweenMutativeRequests)

		if tmpEmptyCommitRequired {
			entry.Logger.Debug("reset empty commit list pull request branch to actual commit", "pr_number", giteaPullRequest.Index, "source_branch", giteaPullRequest.Head.Ref, "actual_commit", prHeadRef)
			if err = worktree.Checkout(&git.CheckoutOptions{
				Create: false,
				Force:  true,
				Branch: plumbing.NewBranchReferenceName(giteaPullRequest.Head.Ref),
			}); err != nil {
				return fmt.Errorf("checking out to-reset empty commit list branch: %v", err)
			}

			if err = worktree.Reset(&git.ResetOptions{Mode: git.HardReset, Commit: plumbing.NewHash(prHeadRef)}); err != nil {
				return fmt.Errorf("reset empty commit list pull request branch: %v", err)
			}

			entry.Logger.Trace("pushing reset empty commit list pull request branch", "repository_id", entry.GiteaRepository.ID, "pr_number", giteaPullRequest.Index)

			if err = entry.GitRepo.PushContext(ctx, &git.PushOptions{
				RemoteName: "github",
				RefSpecs: []config.RefSpec{
					config.RefSpec(fmt.Sprintf("refs/heads/%[1]s:refs/heads/%[1]s", giteaPullRequest.Head.Ref)),
				},
				Force: true,
			}); err != nil {
				if errors.Is(err, git.NoErrAlreadyUpToDate) {
					entry.Logger.Trace("empty commit list branch already exists and is up-to-date on GitHub", "source_branch", giteaPullRequest.Head.Ref, "target_branch", giteaPullRequest.Base.Ref)
				} else {
					return fmt.Errorf("pushing reset temporary empty commit list branch to github: %v", err)
				}
			}
			githubPullRequest.Head.SHA = h.Pointer(prHeadRef)
			if githubPullRequest, _, err = gh.PullRequests.Edit(ctx, entry.GitHubOwner, entry.GitHubRepo, githubPullRequest.GetNumber(), githubPullRequest); err != nil {
				return fmt.Errorf("updating pull request: %v", err)
			}
			time.Sleep(constants.GithubApiPauseBetweenMutativeRequests)

			entry.Logger.Debug("creating empty commit list auto-close comment", "pr_number", giteaPullRequest.Index)
			newComment := github.IssueComment{
				Body: h.Pointer(`> [!CAUTION]
>
> **Due to platform limitations in handling PRs with empty commit history or orphaned commits, this PR was flagged as "merged" or "closed". This does not necessarily represent its original state within Gitea.**`),
			}
			if _, _, err = gh.Issues.CreateComment(ctx, entry.GitHubOwner, entry.GitHubRepo, githubPullRequest.GetNumber(), &newComment); err != nil {
				return fmt.Errorf("creating empty commit list auto-close comment: %v", err)
			}
			time.Sleep(constants.GithubApiPauseBetweenMutativeRequests)
		}

		if giteaPullRequest.State == gitea.StateClosed {
			entry.Logger.Debug("closing pull request", "pr_number", githubPullRequest.GetNumber())

			githubPullRequest.State = h.Pointer("closed")
			if githubPullRequest, _, err = gh.PullRequests.Edit(ctx, entry.GitHubOwner, entry.GitHubRepo, githubPullRequest.GetNumber(), githubPullRequest); err != nil {
				return fmt.Errorf("updating pull request: %v", err)
			}
			time.Sleep(constants.GithubApiPauseBetweenMutativeRequests)
		}

	} else {
		var newState *string
		switch giteaPullRequest.State {
		case "opened":
			newState = h.Pointer("open")
		case "closed", "merged":
			newState = h.Pointer("closed")
		}

		if githubPullRequest.State != nil && newState != nil && *githubPullRequest.State != *newState {
			pullRequestState := &github.PullRequest{
				Number: githubPullRequest.Number,
				State:  newState,
			}

			if githubPullRequest, _, err = gh.PullRequests.Edit(ctx, entry.GitHubOwner, entry.GitHubRepo, pullRequestState.GetNumber(), pullRequestState); err != nil {
				return fmt.Errorf("updating pull request state: %v", err)
			}
			time.Sleep(constants.GithubApiPauseBetweenMutativeRequests)
		}

		if (newState != nil && (githubPullRequest.State == nil || *githubPullRequest.State != *newState)) ||
			(githubPullRequest.Title == nil || *githubPullRequest.Title != giteaPullRequest.Title) ||
			(githubPullRequest.Body == nil || *githubPullRequest.Body != body) ||
			(githubPullRequest.Draft == nil || *githubPullRequest.Draft != giteaPullRequest.Draft) {
			entry.Logger.Info("updating pull request", "pr_number", githubPullRequest.GetNumber())

			githubPullRequest.Title = &giteaPullRequest.Title
			githubPullRequest.Body = &body
			githubPullRequest.Draft = &giteaPullRequest.Draft
			githubPullRequest.MaintainerCanModify = nil
			if githubPullRequest, _, err = gh.PullRequests.Edit(ctx, entry.GitHubOwner, entry.GitHubRepo, githubPullRequest.GetNumber(), githubPullRequest); err != nil {
				return fmt.Errorf("updating pull request: %v", err)
			}
			time.Sleep(constants.GithubApiPauseBetweenMutativeRequests)
		} else {
			entry.Logger.Trace("existing pull request is up-to-date", "pr_number", githubPullRequest.GetNumber())
		}
	}

	if cleanUpBranch {
		entry.Logger.Debug("deleting temporary branches for closed pull request", "pr_number", githubPullRequest.GetNumber(), "source_branch", giteaPullRequest.Head.Ref, "target_branch", giteaPullRequest.Base.Ref)
		if err = entry.GitRepo.PushContext(ctx, &git.PushOptions{
			RemoteName: "github",
			RefSpecs: []config.RefSpec{
				config.RefSpec(fmt.Sprintf(":refs/heads/%s", giteaPullRequest.Head.Ref)),
				config.RefSpec(fmt.Sprintf(":refs/heads/%s", giteaPullRequest.Base.Ref)),
			},
			Force: true,
		}); err != nil {
			if errors.Is(err, git.NoErrAlreadyUpToDate) {
				entry.Logger.Trace("branches already deleted on GitHub", "pr_number", githubPullRequest.GetNumber(), "source_branch", giteaPullRequest.Head.Ref, "target_branch", giteaPullRequest.Base.Ref)
			} else {
				return fmt.Errorf("pushing branch deletions to github: %v", err)
			}
		}
	}

	err = entry.MigrateComments(ctx, giteaPullRequest.Index, githubPullRequest.GetNumber())
	if err != nil {
		return fmt.Errorf("migrating comments: %v", err)
	}
	entry.PRSuccessCount++

	return nil
}

func migrateIssue(ctx context.Context, entry *migration.Entry, initialMigration bool, giteaIssue *gitea.Issue) error {
	var githubIssue *github.Issue

	if !initialMigration {
		entry.Logger.Debug("searching for any existing issue", "issue_number", giteaIssue.Index)
		query := fmt.Sprintf("repo:%s/%s AND is:issue AND \"Gitea Issue Number\" \"[%d]\" in:body", entry.GitHubOwner, entry.GitHubRepo, giteaIssue.Index)
		searchResult, err := getGithubSearchResults(ctx, query)
		if err != nil {
			return fmt.Errorf("listing issues: %v", err)
		}

		for _, issue := range searchResult.Issues {
			if issue == nil {
				continue
			}

			// Check for context cancellation
			if err := ctx.Err(); err != nil {
				return fmt.Errorf("preparing to retrieve issue: %v", err)
			}

			if !issue.IsPullRequest() && strings.Contains(issue.GetBody(), fmt.Sprintf("**Gitea Issue Number** | [%d]", giteaIssue.Index)) {
				entry.Logger.Debug("found existing issue", "issue_number", issue.GetNumber())
				githubIssue = issue
				break
			}
		}
	}

	description := giteaIssue.Body
	if strings.TrimSpace(description) == "" {
		description = "_No description_"
	}

	originalState := ""
	if giteaIssue.State == gitea.StateClosed {
		originalState = "> This issue was originally **closed** on Gitea"
	}

	closeDetails := ""
	if giteaIssue.State == gitea.StateClosed && giteaIssue.Closed != nil {
		closeDetails = fmt.Sprintf("\n> | **Date Originally Closed** | %s |", giteaIssue.Closed.Format(constants.DateFormat))
	}

	body := fmt.Sprintf(`> [!NOTE]
> This issue was migrated from Gitea
>
> |      |      |
> | ---- | ---- |
> | **Original Author** | %[1]s |
> | **Gitea Repository** | [%[3]s/%[4]s](https://%[6]s/%[3]s/%[4]s) |
> | **Gitea Issue** | [%[7]s](https://%[6]s/%[3]s/%[4]s/issues/%[2]d) |
> | **Gitea Issue Number** | [%[2]d](https://%[6]s/%[3]s/%[4]s/issues/%[2]d) |
> | **Date Originally Opened** | %[5]s |%[8]s
> |      |      |
>
%[9]s

## Original Description

%[10]s`,
		h.GetGitHubAccountReference(giteaIssue.Poster),
		giteaIssue.Index,
		entry.GiteaOwner,
		entry.GiteaRepo,
		giteaIssue.Created.Format(constants.DateFormat),
		giteaDomain,
		giteaIssue.Title,
		closeDetails,
		originalState,
		description)

	if len(body) > constants.GithubBodyLimit {
		entry.Logger.Warn("issue body was truncated due to platform limits", "issue_number", giteaIssue.Index)
		body = h.SmartRenovateBodyTruncate(body)
	}

	if githubIssue == nil {
		entry.Logger.Info("creating issue", "issue_number", giteaIssue.Index)
		createdIssue, _, err := gh.Issues.Create(ctx, entry.GitHubOwner, entry.GitHubRepo, &github.IssueRequest{
			Title: &giteaIssue.Title,
			Body:  &body,
		})
		if err != nil {
			return fmt.Errorf("creating issue: %v", err)
		}
		githubIssue = createdIssue
		time.Sleep(constants.GithubApiPauseBetweenMutativeRequests)

		if giteaIssue.State == gitea.StateClosed {
			entry.Logger.Debug("closing issue", "issue_number", githubIssue.GetNumber())
			if _, _, err = gh.Issues.Edit(ctx, entry.GitHubOwner, entry.GitHubRepo, githubIssue.GetNumber(), &github.IssueRequest{State: h.Pointer("closed")}); err != nil {
				return fmt.Errorf("closing issue: %v", err)
			}
			time.Sleep(constants.GithubApiPauseBetweenMutativeRequests)
		}
	} else {
		var newState *string
		if giteaIssue.State == gitea.StateClosed {
			newState = h.Pointer("closed")
		} else {
			newState = h.Pointer("open")
		}

		if githubIssue.State != nil && *githubIssue.State != *newState {
			entry.Logger.Debug("updating issue state", "issue_number", githubIssue.GetNumber(), "state", *newState)
			if _, _, err := gh.Issues.Edit(ctx, entry.GitHubOwner, entry.GitHubRepo, githubIssue.GetNumber(), &github.IssueRequest{State: newState}); err != nil {
				return fmt.Errorf("updating issue state: %v", err)
			}
			time.Sleep(constants.GithubApiPauseBetweenMutativeRequests)
		}

		if (githubIssue.Title == nil || *githubIssue.Title != giteaIssue.Title) ||
			(githubIssue.Body == nil || *githubIssue.Body != body) {
			entry.Logger.Info("updating issue", "issue_number", githubIssue.GetNumber())
			if _, _, err := gh.Issues.Edit(ctx, entry.GitHubOwner, entry.GitHubRepo, githubIssue.GetNumber(), &github.IssueRequest{
				Title: &giteaIssue.Title,
				Body:  &body,
			}); err != nil {
				return fmt.Errorf("updating issue: %v", err)
			}
			time.Sleep(constants.GithubApiPauseBetweenMutativeRequests)
		} else {
			entry.Logger.Trace("existing issue is up-to-date", "issue_number", githubIssue.GetNumber())
		}
	}

	if giteaIssue.Poster.UserName == constants.PhantomItemPoster && giteaIssue.Title == constants.PhantomItemTitle && giteaIssue.Body == constants.PhantomItemBody {
		// phantom items don't exist and therefore have no comments - early exit
		entry.IssueSuccessCount++
		return nil
	}

	err := entry.MigrateComments(ctx, giteaIssue.Index, githubIssue.GetNumber())
	if err != nil {
		return err
	}

	entry.IssueSuccessCount++
	return nil
}
