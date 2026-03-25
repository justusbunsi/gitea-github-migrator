package main

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
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
	"github.com/gofri/go-github-ratelimit/v2/github_ratelimit"
	"github.com/gofri/go-github-ratelimit/v2/github_ratelimit/github_primary_ratelimit"
	"github.com/gofri/go-github-ratelimit/v2/github_ratelimit/github_secondary_ratelimit"
	"github.com/google/go-github/v74/github"
	"github.com/hashicorp/go-hclog"
)

const (
	dateFormat          = "Mon, 2 Jan 2006"
	defaultGithubDomain = "github.com"
	defaultGiteaDomain  = "gitea.com"
	githubBodyLimit     = 58000
)

var loop, report bool
var deleteExistingRepos, enablePullRequests, renameMasterToMain bool
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
	PullRequestsCount int
}

type GitHubError struct {
	Message          string
	DocumentationURL string `json:"documentation_url"`
}

func sendErr(err error) {
	errCount++
	logger.Error(err.Error())
}

func unmarshalResp(resp *http.Response, model interface{}) error {
	if resp == nil {
		return nil
	}

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("parsing response body: %+v", err)
	}
	resp.Body.Close()

	// Trim away a BOM if present
	respBody = bytes.TrimPrefix(respBody, []byte("\xef\xbb\xbf"))

	// In some cases the respBody is empty, but not nil, so don't attempt to unmarshal this
	if len(respBody) == 0 {
		return nil
	}

	// Unmarshal into provided model
	if err := json.Unmarshal(respBody, model); err != nil {
		return fmt.Errorf("unmarshaling response body: %+v", err)
	}

	// Reassign the response body as downstream code may expect it
	resp.Body = io.NopCloser(bytes.NewBuffer(respBody))

	return nil
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
	flag.BoolVar(&renameMasterToMain, "rename-master-to-main", false, "rename master branch to main and update pull requests (incompatible with -rename-trunk-branch)")

	flag.StringVar(&githubDomain, "github-domain", defaultGithubDomain, "specifies the GitHub domain to use")
	flag.StringVar(&githubRepo, "github-repo", "", "the GitHub repository to migrate to")
	flag.StringVar(&githubUser, "github-user", "", "specifies the GitHub user to use, who will author any migrated PRs (required)")
	flag.StringVar(&giteaDomain, "gitea-domain", defaultGiteaDomain, "specifies the Gitea domain to use")
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

	rateLimiter := github_ratelimit.New(nil,
		github_primary_ratelimit.WithLimitDetectedCallback(func(ctx *github_primary_ratelimit.CallbackContext) {
			logger.Warn("🔥 Primary rate limit detected", "category", ctx.Category, "reset_time", ctx.ResetTime)
		}),
		github_secondary_ratelimit.WithLimitDetectedCallback(func(ctx *github_secondary_ratelimit.CallbackContext) {
			logger.Warn("🔥 Secondary rate limit detected", "reset_time", ctx.ResetTime, "total_sleep_time", ctx.TotalSleepTime)
		}),
	)
	client := githubpagination.NewClient(rateLimiter, githubpagination.WithPerPage(100))

	if githubDomain == defaultGithubDomain {
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

func parseProjectSlugs(proj []string) ([]string, []string, error) {
	if len(proj) != 2 {
		return nil, nil, fmt.Errorf("too many fields")
	}

	delimPosition := strings.LastIndex(proj[0], "/")
	giteaPath := []string{
		proj[0][:delimPosition],
		proj[0][delimPosition+1:],
	}
	githubPath := strings.Split(proj[1], "/")

	if len(giteaPath) != 2 {
		return nil, nil, fmt.Errorf("invalid Gitea project: %s", proj[0])
	}
	if len(githubPath) != 2 {
		return nil, nil, fmt.Errorf("invalid GitHub project: %s", proj[1])
	}

	return giteaPath, githubPath, nil
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
			errCount++
			sendErr(err)
		}

		if result != nil {
			results = append(results, *result)
		}
	}

	fmt.Println()

	totalPullRequests := 0
	for _, result := range results {
		totalPullRequests += result.PullRequestsCount
		fmt.Printf("%#v\n", result)
	}

	fmt.Println()
	fmt.Printf("Total pull requests: %d\n", totalPullRequests)
	fmt.Println()
}

func reportProject(ctx context.Context, proj []string) (*Report, error) {
	giteaPath, _, err := parseProjectSlugs(proj)
	if err != nil {
		return nil, fmt.Errorf("parsing project slugs: %v", err)
	}

	pullRequests, err := getAllGiteaPullRequests(ctx, giteaPath[0], giteaPath[1])
	if err != nil {
		return nil, err
	}

	return &Report{
		OwnerName:         giteaPath[0],
		RepoName:          giteaPath[1],
		PullRequestsCount: len(pullRequests),
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
					errCount++
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
	giteaPath, githubPath, err := parseProjectSlugs(proj)
	if err != nil {
		return fmt.Errorf("parsing project slugs: %v", err)
	}

	logger.Info("searching for Gitea repository", "owner", giteaPath[0], "repo", giteaPath[1])
	giteaRepository, _, err := gi.GetRepo(giteaPath[0], giteaPath[1])
	if err != nil {
		return fmt.Errorf("retrieve gitea repository: %v", err)
	}

	if giteaRepository == nil {
		return fmt.Errorf("no matching Gitea repository found: %s", proj[0])
	}

	cloneUrl, err := url.Parse(giteaRepository.CloneURL)
	if err != nil {
		return fmt.Errorf("parsing clone URL: %v", err)
	}

	logger.Info("mirroring repository from Gitea to GitHub", "gitea_owner", giteaPath[0], "gitea_repo", giteaPath[1], "github_owner", githubPath[0], "github_repo", githubPath[1])

	user, err := getGithubUser(ctx, githubPath[0])
	if err != nil {
		return fmt.Errorf("retrieving github user: %v", err)
	}

	var org string
	if strings.EqualFold(*user.Type, "organization") {
		org = githubPath[0]
	} else if !strings.EqualFold(*user.Type, "user") || !strings.EqualFold(*user.Login, githubPath[0]) {
		return fmt.Errorf("configured owner is neither an organization nor the current user: %s", githubPath[0])
	}

	logger.Debug("checking for existing repository on GitHub", "owner", githubPath[0], "repo", githubPath[1])
	_, _, err = gh.Repositories.Get(ctx, githubPath[0], githubPath[1])

	var githubError *github.ErrorResponse
	if err != nil && (!errors.As(err, &githubError) || githubError == nil || githubError.Response == nil || githubError.Response.StatusCode != http.StatusNotFound) {
		return fmt.Errorf("retrieving github repo: %v", err)
	}

	var createRepo, repoDeleted bool
	if err != nil {
		createRepo = true
	} else if deleteExistingRepos {
		logger.Warn("existing repository was found on GitHub, proceeding to delete", "owner", githubPath[0], "repo", githubPath[1])
		if _, err = gh.Repositories.Delete(ctx, githubPath[0], githubPath[1]); err != nil {
			return fmt.Errorf("deleting existing github repo: %v", err)
		}

		createRepo = true
		repoDeleted = true
	}

	defaultBranch := "main"
	if renameTrunkBranch != "" {
		defaultBranch = renameTrunkBranch
	} else if !renameMasterToMain && giteaRepository.DefaultBranch != "" {
		defaultBranch = giteaRepository.DefaultBranch
	}

	homepage := fmt.Sprintf("https://%s/%s/%s", giteaDomain, giteaPath[0], giteaPath[1])

	if createRepo {
		if repoDeleted {
			logger.Warn("recreating GitHub repository", "owner", githubPath[0], "repo", githubPath[1])
		} else {
			logger.Debug("repository not found on GitHub, proceeding to create", "owner", githubPath[0], "repo", githubPath[1])
		}
		newRepo := github.Repository{
			Name:          pointer(githubPath[1]),
			Description:   &giteaRepository.Description,
			Homepage:      &homepage,
			DefaultBranch: &defaultBranch,
			Private:       pointer(true),
			HasIssues:     pointer(true),
			HasProjects:   pointer(true),
			HasWiki:       pointer(true),
		}
		if _, _, err = gh.Repositories.Create(ctx, org, &newRepo); err != nil {
			return fmt.Errorf("creating github repo: %v", err)
		}
	}

	logger.Debug("updating repository settings", "owner", githubPath[0], "repo", githubPath[1])
	description := regexp.MustCompile("\r|\n").ReplaceAllString(giteaRepository.Description, " ")
	updateRepo := github.Repository{
		Name:              pointer(githubPath[1]),
		Description:       &description,
		Homepage:          &homepage,
		AllowAutoMerge:    pointer(true),
		AllowMergeCommit:  pointer(true),
		AllowRebaseMerge:  pointer(true),
		AllowSquashMerge:  pointer(true),
		AllowUpdateBranch: pointer(true),
	}
	if _, _, err = gh.Repositories.Edit(ctx, githubPath[0], githubPath[1], &updateRepo); err != nil {
		return fmt.Errorf("updating github repo: %v", err)
	}

	cloneUrl.User = url.UserPassword("oauth2", giteaToken)
	cloneUrlWithCredentials := cloneUrl.String()

	// In-memory filesystem for worktree operations
	fs := memfs.New()

	logger.Debug("cloning repository", "owner", giteaPath[0], "repo", giteaPath[1], "url", giteaRepository.CloneURL)
	gitRepo, err := git.CloneContext(ctx, memory.NewStorage(), fs, &git.CloneOptions{
		URL:        cloneUrlWithCredentials,
		Auth:       nil,
		RemoteName: "gitea",
		Mirror:     true,
	})
	if err != nil {
		return fmt.Errorf("cloning gitea repo: %v", err)
	}

	if defaultBranch != giteaRepository.DefaultBranch {
		if giteaTrunk, err := gitRepo.Reference(plumbing.NewBranchReferenceName(giteaRepository.DefaultBranch), false); err == nil {
			logger.Info("renaming trunk branch prior to push", "owner", giteaPath[0], "repo", giteaPath[1], "gitea_trunk", giteaRepository.DefaultBranch, "github_trunk", defaultBranch, "sha", giteaTrunk.Hash())

			logger.Debug("creating new trunk branch", "owner", giteaPath[0], "repo", giteaPath[1], "github_trunk", defaultBranch, "sha", giteaTrunk.Hash())
			githubTrunk := plumbing.NewHashReference(plumbing.NewBranchReferenceName(defaultBranch), giteaTrunk.Hash())
			if err = gitRepo.Storer.SetReference(githubTrunk); err != nil {
				return fmt.Errorf("creating trunk branch: %v", err)
			}

			logger.Debug("deleting old trunk branch", "owner", giteaPath[0], "repo", giteaPath[1], "gitea_trunk", giteaRepository.DefaultBranch, "sha", giteaTrunk.Hash())
			if err = gitRepo.Storer.RemoveReference(giteaTrunk.Name()); err != nil {
				return fmt.Errorf("deleting old trunk branch: %v", err)
			}
		}
	}

	githubUrl := fmt.Sprintf("https://%s/%s/%s", githubDomain, githubPath[0], githubPath[1])
	githubUrlWithCredentials := fmt.Sprintf("https://%s:%s@%s/%s/%s", githubUser, githubToken, githubDomain, githubPath[0], githubPath[1])

	logger.Debug("adding remote for GitHub repository", "owner", giteaPath[0], "repo", giteaPath[1], "url", githubUrl)
	if _, err = gitRepo.CreateRemote(&config.RemoteConfig{
		Name:   "github",
		URLs:   []string{githubUrlWithCredentials},
		Mirror: true,
	}); err != nil {
		return fmt.Errorf("adding github remote: %v", err)
	}

	logger.Debug("force-pushing to GitHub repository", "owner", giteaPath[0], "repo", giteaPath[1], "url", githubUrl)
	if err = gitRepo.PushContext(ctx, &git.PushOptions{
		RemoteName: "github",
		Force:      true,
		//Prune:      true, // causes error, attempts to delete main branch
	}); err != nil {
		if errors.Is(err, git.NoErrAlreadyUpToDate) {
			logger.Debug("repository already up-to-date on GitHub", "owner", giteaPath[0], "repo", giteaPath[1], "url", githubUrl)
		} else {
			return fmt.Errorf("pushing to github repo: %v", err)
		}
	}

	logger.Debug("pushing tags to GitHub repository", "owner", giteaPath[0], "repo", giteaPath[1], "url", githubUrl)
	if err = gitRepo.PushContext(ctx, &git.PushOptions{
		RemoteName: "github",
		Force:      true,
		RefSpecs:   []config.RefSpec{"refs/tags/*:refs/tags/*"},
	}); err != nil {
		if errors.Is(err, git.NoErrAlreadyUpToDate) {
			logger.Debug("repository already up-to-date on GitHub", "owner", giteaPath[0], "repo", giteaPath[1], "url", githubUrl)
		} else {
			return fmt.Errorf("pushing to github repo: %v", err)
		}
	}

	logger.Debug("setting default repository branch", "owner", githubPath[0], "repo", githubPath[1], "branch_name", defaultBranch)
	updateRepo = github.Repository{
		DefaultBranch: &defaultBranch,
	}
	if _, _, err = gh.Repositories.Edit(ctx, githubPath[0], githubPath[1], &updateRepo); err != nil {
		return fmt.Errorf("setting default branch: %v", err)
	}

	if enablePullRequests {
		migratePullRequests(ctx, githubPath, giteaPath, defaultBranch, giteaRepository, gitRepo, createRepo)
	}

	return nil
}

func migratePullRequests(ctx context.Context, githubPath, giteaPath []string, defaultBranch string, giteaRepository *gitea.Repository, gitRepo *git.Repository, initialMigration bool) {
	giteaPullRequests, err := getAllGiteaPullRequests(ctx, giteaPath[0], giteaPath[1])
	if err != nil {
		sendErr(err)
		return
	}

	var successCount, failureCount int
	totalCount := len(giteaPullRequests)
	logger.Info("migrating pull requests from Gitea to GitHub", "owner", giteaPath[0], "repo", giteaPath[1], "count", totalCount)
	for _, giteaPullRequest := range giteaPullRequests {
		if giteaPullRequest == nil {
			continue
		}

		// Check for context cancellation
		if err := ctx.Err(); err != nil {
			sendErr(fmt.Errorf("preparing to list pull requests: %v", err))
			break
		}

		if giteaPullRequest.MergeBase == "" {
			sendErr(fmt.Errorf("identifying suitable merge base for pull request %d", giteaPullRequest.Index))
			failureCount++
		}

		sourceBranchForClosedPullRequest := fmt.Sprintf("migration-source-%d/%s", giteaPullRequest.Index, giteaPullRequest.Head.Ref)
		targetBranchForClosedPullRequest := fmt.Sprintf("migration-target-%d/%s", giteaPullRequest.Index, giteaPullRequest.Base.Ref)

		var cleanUpBranch, tmpEmptyCommitRequired bool
		var githubPullRequest *github.PullRequest
		logger.Trace("retrieve pull request head ref", "owner", giteaPath[0], "repo", giteaPath[1], "pr_number", giteaPullRequest.Index)
		prHeadRefs, _, err := gi.GetRepoRefs(giteaPath[0], giteaPath[1], fmt.Sprintf("pull/%d/head", giteaPullRequest.Index))
		if err != nil {
			sendErr(fmt.Errorf("retrieve head ref for pull request %d: %v", giteaPullRequest.Index, err))
			failureCount++
			continue
		}
		if len(prHeadRefs) == 0 {
			sendErr(fmt.Errorf("no head ref for pull request %d found", giteaPullRequest.Index))
			failureCount++
			continue
		}
		prHeadRef := prHeadRefs[0].Object.SHA

		// Some pull requests have no commits, flag these for later handling
		if strings.EqualFold(giteaPullRequest.MergeBase, prHeadRef) {
			logger.Debug("pull request with empty commit list", "owner", giteaPath[0], "repo", giteaPath[1], "pr_number", giteaPullRequest.Index)
			tmpEmptyCommitRequired = true
		}

		if !initialMigration {
			logger.Debug("searching for any existing pull request", "owner", githubPath[0], "repo", githubPath[1], "pr_number", giteaPullRequest.Index)
			sourceBranches := []string{giteaPullRequest.Head.Ref, sourceBranchForClosedPullRequest}
			branchQuery := fmt.Sprintf("head:%s", strings.Join(sourceBranches, " OR head:"))
			query := fmt.Sprintf("repo:%s/%s AND is:pr AND (%s)", githubPath[0], githubPath[1], branchQuery)
			searchResult, err := getGithubSearchResults(ctx, query)
			if err != nil {
				sendErr(fmt.Errorf("listing pull requests: %v", err))
				continue
			}

			// Look for an existing GitHub pull request
			skip := false
			for _, issue := range searchResult.Issues {
				if issue == nil {
					continue
				}

				// Check for context cancellation
				if err := ctx.Err(); err != nil {
					sendErr(fmt.Errorf("preparing to retrieve pull request: %v", err))
					break
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
						ghPr, err := getGithubPullRequest(ctx, githubPath[0], githubPath[1], prNumber)
						if err != nil {
							sendErr(fmt.Errorf("retrieving pull request: %v", err))
							skip = true
							break
						}

						if strings.Contains(ghPr.GetBody(), fmt.Sprintf("**Gitea PR Number** | %d", giteaPullRequest.Index)) ||
							strings.Contains(ghPr.GetBody(), fmt.Sprintf("**Gitea PR Number** | [%d]", giteaPullRequest.Index)) {
							logger.Debug("found existing pull request", "owner", githubPath[0], "repo", githubPath[1], "pr_number", ghPr.GetNumber())
							githubPullRequest = ghPr
							break
						}
					}
				}
			}
			if skip {
				continue
			}
		}

		worktree, err := gitRepo.Worktree()
		if err != nil {
			sendErr(fmt.Errorf("creating worktree: %v", err))
			failureCount++
			continue
		}

		if githubPullRequest == nil {
			logger.Trace("loading pull request head commit", "owner", giteaPath[0], "repo", giteaPath[1], "pr_number", giteaPullRequest.Index, "head_sha", prHeadRef)
			prHeadHash := plumbing.NewHash(prHeadRef)
			prHeadCommit, err := object.GetCommit(gitRepo.Storer, prHeadHash)
			if err != nil {
				sendErr(fmt.Errorf("loading pull request head commit: %v", err))
				failureCount++
				continue
			}
			logger.Trace("loading merge base", "owner", giteaPath[0], "repo", giteaPath[1], "pr_number", giteaPullRequest.Index, "pr_merge_base", giteaPullRequest.MergeBase)
			mergeBaseHash := plumbing.NewHash(giteaPullRequest.MergeBase)
			mergeBaseCommit, err := object.GetCommit(gitRepo.Storer, mergeBaseHash)
			if err != nil {
				sendErr(fmt.Errorf("loading merge base: %v", err))
				failureCount++
				continue
			}
			logger.Trace("detecting best common ancestor", "owner", giteaPath[0], "repo", giteaPath[1], "pr_number", giteaPullRequest.Index, "base", mergeBaseHash, "head", prHeadHash)
			bases, err := mergeBaseCommit.MergeBase(prHeadCommit)
			if err != nil {
				sendErr(fmt.Errorf("detecting best common ancestor: %v", err))
				failureCount++
				continue
			}
			if len(bases) == 0 {
				logger.Trace("orphaned head commit detected", "owner", giteaPath[0], "repo", giteaPath[1], "pr_number", giteaPullRequest.Index, "sha", prHeadHash)
				tmpEmptyCommitRequired = true
			}

			if tmpEmptyCommitRequired {
				logger.Trace("checkout source branch for empty commit list pull request", "owner", giteaPath[0], "repo", giteaPath[1], "repository_id", giteaRepository.ID, "pr_number", giteaPullRequest.Index)
				if err = worktree.Checkout(&git.CheckoutOptions{
					Create: conditional(giteaPullRequest.State == gitea.StateOpen, false, true),
					Force:  true,
					Branch: plumbing.NewBranchReferenceName(conditional(giteaPullRequest.State == gitea.StateClosed, sourceBranchForClosedPullRequest, giteaPullRequest.Head.Ref)),
				}); err != nil {
					sendErr(fmt.Errorf("checking out temporary empty commit list source branch for pull request #%d: %v", giteaPullRequest.Index, err))
					failureCount++
					continue
				}

				resetHash := conditional(len(bases) == 0, mergeBaseHash, prHeadHash)
				logger.Trace("reset worktree for migrator-required empty commit", "owner", giteaPath[0], "repo", giteaPath[1], "pr_number", giteaPullRequest.Index, "reset_to_sha", resetHash)
				if err = worktree.Reset(&git.ResetOptions{Mode: git.HardReset, Commit: resetHash}); err != nil {
					sendErr(fmt.Errorf("reset empty commit list pull request branch: %v", err))
					failureCount++
					continue
				}

				logger.Debug("creating empty migration commit", "pr_number", giteaPullRequest.Index)
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
					sendErr(fmt.Errorf("creating empty migration commit: %v", err))
					failureCount++
					continue
				}

				if strings.EqualFold(string(giteaPullRequest.State), string(gitea.StateOpen)) {
					logger.Trace("pushing source branch for empty commit list pull request", "owner", giteaPath[0], "repo", giteaPath[1], "repository_id", giteaRepository.ID, "pr_number", giteaPullRequest.Index)

					if err = gitRepo.PushContext(ctx, &git.PushOptions{
						RemoteName: "github",
						RefSpecs: []config.RefSpec{
							config.RefSpec(fmt.Sprintf("refs/heads/%[1]s:refs/heads/%[1]s", giteaPullRequest.Head.Ref)),
						},
						Force: true,
					}); err != nil {
						if errors.Is(err, git.NoErrAlreadyUpToDate) {
							logger.Trace("empty commit list branch already exists and is up-to-date on GitHub", "owner", githubPath[0], "repo", githubPath[1], "source_branch", giteaPullRequest.Head.Ref, "target_branch", giteaPullRequest.Base.Ref)
						} else {
							sendErr(fmt.Errorf("pushing temporary empty commit list branch to github: %v", err))
							failureCount++
							continue
						}
					}
				}
			}

			// Proceed to create temporary branches when migrating a merged/closed merge request that doesn't yet have a counterpart PR in GitHub (can't create one without a branch)
			if !strings.EqualFold(string(giteaPullRequest.State), string(gitea.StateOpen)) {
				logger.Trace("searching for existing branch for closed/merged pull request", "owner", giteaPath[0], "repo", giteaPath[1], "repository_id", giteaRepository.ID, "pr_number", giteaPullRequest.Index, "source_branch", giteaPullRequest.Head.Ref)

				// Generate temporary branch names
				giteaPullRequest.Head.Ref = sourceBranchForClosedPullRequest
				giteaPullRequest.Base.Ref = targetBranchForClosedPullRequest

				logger.Trace("creating target branch for merged/closed pull request", "owner", giteaPath[0], "repo", giteaPath[1], "repository_id", giteaRepository.ID, "pr_number", giteaPullRequest.Index, "branch", giteaPullRequest.Base.Ref, "sha", mergeBaseCommit.Hash)
				if err = worktree.Checkout(&git.CheckoutOptions{
					Create: true,
					Force:  true,
					Branch: plumbing.NewBranchReferenceName(giteaPullRequest.Base.Ref),
					Hash:   mergeBaseCommit.Hash,
				}); err != nil {
					sendErr(fmt.Errorf("checking out temporary target branch: %v", err))
					failureCount++
					continue
				}

				logger.Trace("creating source branch for merged/closed pull request", "owner", giteaPath[0], "repo", giteaPath[1], "repository_id", giteaRepository.ID, "pr_number", giteaPullRequest.Index, "branch", giteaPullRequest.Head.Ref, "sha", prHeadHash)
				if err = worktree.Checkout(&git.CheckoutOptions{
					Create: conditional(tmpEmptyCommitRequired, false, true),
					Force:  true,
					Branch: plumbing.NewBranchReferenceName(giteaPullRequest.Head.Ref),
					Hash:   conditional(tmpEmptyCommitRequired, plumbing.ZeroHash, prHeadHash),
				}); err != nil {
					sendErr(fmt.Errorf("checking out temporary source branch: %v", err))
					failureCount++
					continue
				}

				logger.Debug("pushing branches for merged/closed pull request", "owner", githubPath[0], "repo", githubPath[1], "source_branch", giteaPullRequest.Head.Ref, "target_branch", giteaPullRequest.Base.Ref)
				if err = gitRepo.PushContext(ctx, &git.PushOptions{
					RemoteName: "github",
					RefSpecs: []config.RefSpec{
						config.RefSpec(fmt.Sprintf("refs/heads/%[1]s:refs/heads/%[1]s", giteaPullRequest.Head.Ref)),
						config.RefSpec(fmt.Sprintf("refs/heads/%[1]s:refs/heads/%[1]s", giteaPullRequest.Base.Ref)),
					},
					Force: true,
				}); err != nil {
					if errors.Is(err, git.NoErrAlreadyUpToDate) {
						logger.Trace("branch already exists and is up-to-date on GitHub", "owner", githubPath[0], "repo", githubPath[1], "source_branch", giteaPullRequest.Head.Ref, "target_branch", giteaPullRequest.Base.Ref)
					} else {
						sendErr(fmt.Errorf("pushing temporary branches to github: %v", err))
						failureCount++
						continue
					}
				}

				// We will clean up these temporary branches after configuring and closing the pull request
				cleanUpBranch = true
			}
		}

		if defaultBranch != giteaRepository.DefaultBranch && giteaPullRequest.Base.Ref == giteaRepository.DefaultBranch {
			logger.Trace("changing target trunk branch", "owner", giteaPath[0], "repo", giteaPath[1], "repository_id", giteaRepository.ID, "pr_number", giteaPullRequest.Index, "old_trunk", giteaRepository.DefaultBranch, "new_trunk", defaultBranch)
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

		logger.Debug("determining pull request approvers", "owner", giteaPath[0], "repo", giteaPath[1], "repository_id", giteaRepository.ID, "pr_number", giteaPullRequest.Index)
		approvers := make([]string, 0)
		reviews, _, err := gi.ListPullReviews(giteaPath[0], giteaPath[1], giteaPullRequest.Index, gitea.ListPullReviewsOptions{})
		if err != nil {
			sendErr(fmt.Errorf("listing pull request reviews: %v", err))
		} else {
			for _, review := range reviews {
				if review.State == gitea.ReviewStateApproved {
					approvers = append(approvers, getGitHubAccountReference(review.Reviewer))
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
				closeDetails = fmt.Sprintf("\n> | **Date Originally Merged** | %s |\n> | **Original Merger** | %s |\n> | **Merge Commit** | %s |", giteaPullRequest.Merged.Format(dateFormat), giteaPullRequest.MergedBy.UserName, *giteaPullRequest.MergedCommitID)
			} else if giteaPullRequest.Closed != nil {
				closeDetails = fmt.Sprintf("\n> | **Date Originally Closed** | %s |", giteaPullRequest.Closed.Format(dateFormat))
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

%[3]s`, getGitHubAccountReference(giteaPullRequest.Poster), giteaPullRequest.Index, description, giteaPath[0], giteaPath[1], giteaPullRequest.Created.Format(dateFormat), closeDetails, approval, originalState, giteaDomain, giteaPullRequest.Title)

		if len(body) > githubBodyLimit {
			logger.Warn("pull request body was truncated due to platform limits", "owner", githubPath[0], "repo", githubPath[1], "source_branch", giteaPullRequest.Head.Ref, "target_branch", giteaPullRequest.Base.Ref)
			body = smartRenovateBodyTruncate(body)
		}

		if githubPullRequest == nil {
			logger.Info("creating pull request", "owner", githubPath[0], "repo", githubPath[1], "source_branch", giteaPullRequest.Head.Ref, "target_branch", giteaPullRequest.Base.Ref)
			newPullRequest := github.NewPullRequest{
				Title:               &giteaPullRequest.Title,
				Head:                &giteaPullRequest.Head.Ref,
				Base:                &giteaPullRequest.Base.Ref,
				Body:                &body,
				MaintainerCanModify: pointer(true),
				Draft:               &giteaPullRequest.Draft,
			}
			if githubPullRequest, _, err = gh.PullRequests.Create(ctx, githubPath[0], githubPath[1], &newPullRequest); err != nil {
				sendErr(fmt.Errorf("creating pull request: %v", err))
				failureCount++
				continue
			}

			if tmpEmptyCommitRequired {
				logger.Debug("reset empty commit list pull request branch to actual commit", "pr_number", giteaPullRequest.Index, "source_branch", giteaPullRequest.Head.Ref, "actual_commit", prHeadRef)
				if err = worktree.Checkout(&git.CheckoutOptions{
					Create: false,
					Force:  true,
					Branch: plumbing.NewBranchReferenceName(giteaPullRequest.Head.Ref),
				}); err != nil {
					sendErr(fmt.Errorf("checking out to-reset empty commit list branch: %v", err))
					failureCount++
					continue
				}

				if err = worktree.Reset(&git.ResetOptions{Mode: git.HardReset, Commit: plumbing.NewHash(prHeadRef)}); err != nil {
					sendErr(fmt.Errorf("reset empty commit list pull request branch: %v", err))
					failureCount++
					continue
				}

				logger.Trace("pushing reset empty commit list pull request branch", "owner", giteaPath[0], "repo", giteaPath[1], "repository_id", giteaRepository.ID, "pr_number", giteaPullRequest.Index)

				if err = gitRepo.PushContext(ctx, &git.PushOptions{
					RemoteName: "github",
					RefSpecs: []config.RefSpec{
						config.RefSpec(fmt.Sprintf("refs/heads/%[1]s:refs/heads/%[1]s", giteaPullRequest.Head.Ref)),
					},
					Force: true,
				}); err != nil {
					if errors.Is(err, git.NoErrAlreadyUpToDate) {
						logger.Trace("empty commit list branch already exists and is up-to-date on GitHub", "owner", githubPath[0], "repo", githubPath[1], "source_branch", giteaPullRequest.Head.Ref, "target_branch", giteaPullRequest.Base.Ref)
					} else {
						sendErr(fmt.Errorf("pushing reset temporary empty commit list branch to github: %v", err))
						failureCount++
						continue
					}
				}
				githubPullRequest.Head.SHA = pointer(prHeadRef)
				if githubPullRequest, _, err = gh.PullRequests.Edit(ctx, githubPath[0], githubPath[1], githubPullRequest.GetNumber(), githubPullRequest); err != nil {
					sendErr(fmt.Errorf("updating pull request: %v", err))
					failureCount++
					continue
				}

				logger.Debug("creating empty commit list auto-close comment", "owner", githubPath[0], "repo", githubPath[1], "pr_number", giteaPullRequest.Index)
				newComment := github.IssueComment{
					Body: pointer(`> [!CAUTION]
>
> **Due to platform limitations in handling PRs with empty commit history or orphaned commits, this PR was flagged as "merged" or "closed". This does not necessarily represent its original state within Gitea.**`),
				}
				if _, _, err = gh.Issues.CreateComment(ctx, githubPath[0], githubPath[1], githubPullRequest.GetNumber(), &newComment); err != nil {
					sendErr(fmt.Errorf("creating empty commit list auto-close comment: %v", err))
					failureCount++
				}
			}

			if giteaPullRequest.State == gitea.StateClosed {
				logger.Debug("closing pull request", "owner", githubPath[0], "repo", githubPath[1], "pr_number", githubPullRequest.GetNumber())

				githubPullRequest.State = pointer("closed")
				if githubPullRequest, _, err = gh.PullRequests.Edit(ctx, githubPath[0], githubPath[1], githubPullRequest.GetNumber(), githubPullRequest); err != nil {
					sendErr(fmt.Errorf("updating pull request: %v", err))
					failureCount++
					continue
				}
			}

		} else {
			var newState *string
			switch giteaPullRequest.State {
			case "opened":
				newState = pointer("open")
			case "closed", "merged":
				newState = pointer("closed")
			}

			if githubPullRequest.State != nil && newState != nil && *githubPullRequest.State != *newState {
				pullRequestState := &github.PullRequest{
					Number: githubPullRequest.Number,
					State:  newState,
				}

				if githubPullRequest, _, err = gh.PullRequests.Edit(ctx, githubPath[0], githubPath[1], pullRequestState.GetNumber(), pullRequestState); err != nil {
					sendErr(fmt.Errorf("updating pull request state: %v", err))
					failureCount++
					continue
				}
			}

			if (newState != nil && (githubPullRequest.State == nil || *githubPullRequest.State != *newState)) ||
				(githubPullRequest.Title == nil || *githubPullRequest.Title != giteaPullRequest.Title) ||
				(githubPullRequest.Body == nil || *githubPullRequest.Body != body) ||
				(githubPullRequest.Draft == nil || *githubPullRequest.Draft != giteaPullRequest.Draft) {
				logger.Info("updating pull request", "owner", githubPath[0], "repo", githubPath[1], "pr_number", githubPullRequest.GetNumber())

				githubPullRequest.Title = &giteaPullRequest.Title
				githubPullRequest.Body = &body
				githubPullRequest.Draft = &giteaPullRequest.Draft
				githubPullRequest.MaintainerCanModify = nil
				if githubPullRequest, _, err = gh.PullRequests.Edit(ctx, githubPath[0], githubPath[1], githubPullRequest.GetNumber(), githubPullRequest); err != nil {
					sendErr(fmt.Errorf("updating pull request: %v", err))
					failureCount++
					continue
				}
			} else {
				logger.Trace("existing pull request is up-to-date", "owner", githubPath[0], "repo", githubPath[1], "pr_number", githubPullRequest.GetNumber())
			}
		}

		if cleanUpBranch {
			logger.Debug("deleting temporary branches for closed pull request", "owner", githubPath[0], "repo", githubPath[1], "pr_number", githubPullRequest.GetNumber(), "source_branch", giteaPullRequest.Head.Ref, "target_branch", giteaPullRequest.Base.Ref)
			if err = gitRepo.PushContext(ctx, &git.PushOptions{
				RemoteName: "github",
				RefSpecs: []config.RefSpec{
					config.RefSpec(fmt.Sprintf(":refs/heads/%s", giteaPullRequest.Head.Ref)),
					config.RefSpec(fmt.Sprintf(":refs/heads/%s", giteaPullRequest.Base.Ref)),
				},
				Force: true,
			}); err != nil {
				if errors.Is(err, git.NoErrAlreadyUpToDate) {
					logger.Trace("branches already deleted on GitHub", "owner", githubPath[0], "repo", githubPath[1], "pr_number", githubPullRequest.GetNumber(), "source_branch", giteaPullRequest.Head.Ref, "target_branch", giteaPullRequest.Base.Ref)
				} else {
					sendErr(fmt.Errorf("pushing branch deletions to github: %v", err))
					failureCount++
					continue
				}
			}
		}

		err = migrateItemComments(ctx, githubPath, giteaPath, giteaRepository, giteaPullRequest.Index, githubPullRequest.GetNumber())
		if err != nil {
			sendErr(err)
			failureCount++
		} else {
			successCount++
		}
	}

	skippedCount := totalCount - successCount - failureCount

	logger.Info("migrated pull requests from Gitea to GitHub", "owner", giteaPath[0], "repo", giteaPath[1], "successful", successCount, "failed", failureCount, "skipped", skippedCount)
}

func migrateItemComments(ctx context.Context, githubPath, giteaPath []string, giteaRepository *gitea.Repository, giteaItemId int64, githubItemId int) error {
	var giteaComments []*gitea.Comment
	opts := &gitea.ListIssueCommentOptions{}

	logger.Debug("retrieving Gitea comments", "owner", giteaPath[0], "repo", giteaPath[1], "repository_id", giteaRepository.ID, "item_id", giteaItemId)
	for {
		result, resp, err := gi.ListIssueComments(giteaPath[0], giteaPath[1], giteaItemId, gitea.ListIssueCommentOptions{})
		if err != nil {
			return fmt.Errorf("listing gitea comments: %v", err)
		}

		giteaComments = append(giteaComments, result...)

		if resp.NextPage == 0 {
			break
		}

		opts.Page = resp.NextPage
	}

	logger.Info("migrating comments from Gitea to GitHub", "owner", githubPath[0], "repo", githubPath[1], "item_id", githubItemId, "count", len(giteaComments))

	if len(giteaComments) == 0 {
		// We don't need to request GitHub API if there are no comments to be migrated at all. Those secondary rate limit points can be safed.
		return nil
	}

	if githubItemId == 0 {
		return fmt.Errorf("GitHub item id is 0 and would cause the API to retrieve all comments across the repository - leading to high rate limit burning")
	}

	logger.Debug("retrieving GitHub comments", "owner", githubPath[0], "repo", githubPath[1], "item_id", githubItemId)
	githubComments, _, err := gh.Issues.ListComments(ctx, githubPath[0], githubPath[1], githubItemId, &github.IssueListCommentsOptions{Sort: pointer("created"), Direction: pointer("asc")})
	if err != nil {
		return fmt.Errorf("listing github comments: %v", err)
	}

	for _, comment := range giteaComments {
		if comment == nil {
			continue
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

%[4]s`, getGitHubAccountReference(comment.Poster), comment.ID, comment.Created.Format(dateFormat), comment.Body)
		if len(commentBody) > githubBodyLimit {
			logger.Warn("comment was truncated due to platform limits", "owner", githubPath[0], "repo", githubPath[1], "gitea_item", giteaItemId, "github_item", githubItemId, "comment_id", comment.ID)
			commentBody = strings.ReplaceAll(commentBody, "This comment was migrated from Gitea", "This comment was migrated from Gitea **and was truncated due to platform limits**")
			commentBody = commentBody[:githubBodyLimit] + "..."
		}

		foundExistingComment := false
		for _, githubComment := range githubComments {
			if githubComment == nil {
				continue
			}

			if strings.Contains(githubComment.GetBody(), fmt.Sprintf("**Comment ID** | %d", comment.ID)) {
				foundExistingComment = true

				if githubComment.Body == nil || *githubComment.Body != commentBody {
					logger.Debug("updating comment", "owner", githubPath[0], "repo", githubPath[1], "item_id", githubItemId, "comment_id", githubComment.GetID())
					githubComment.Body = &commentBody
					if _, _, err = gh.Issues.EditComment(ctx, githubPath[0], githubPath[1], githubComment.GetID(), githubComment); err != nil {
						// TODO: think about whether to allow "!foundExistingComment" branch to create a new comment on error; previously loop-break instead of return
						return fmt.Errorf("updating comments: %v", err)
					}
				}
			} else {
				logger.Trace("existing comment is up-to-date", "owner", githubPath[0], "repo", githubPath[1], "item_id", githubItemId, "comment_id", githubComment.GetID())
			}
		}

		if !foundExistingComment {
			logger.Debug("creating comment", "owner", githubPath[0], "repo", githubPath[1], "item_id", githubItemId)
			newComment := github.IssueComment{
				Body: &commentBody,
			}
			if _, _, err = gh.Issues.CreateComment(ctx, githubPath[0], githubPath[1], githubItemId, &newComment); err != nil {
				return fmt.Errorf("creating comment for gitea item #%d (#%d): %v", giteaItemId, githubItemId, err)
			}
		}
	}

	return nil
}
