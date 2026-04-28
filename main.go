package main

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/csv"
	"encoding/hex"
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
	"github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/go-git/go-git/v5/storage/memory"
	"github.com/google/go-github/v74/github"
	"github.com/hashicorp/go-hclog"
	"github.com/justusbunsi/gitea-github-migrator/internal/constants"
	"github.com/justusbunsi/gitea-github-migrator/internal/github_client"
	h "github.com/justusbunsi/gitea-github-migrator/internal/helpers"
	"github.com/justusbunsi/gitea-github-migrator/internal/migration"
	"github.com/justusbunsi/gitea-github-migrator/internal/retry_client"
	"github.com/justusbunsi/progress-bar/progressbar"
)

var loop, report bool
var deleteExistingRepos, enablePullRequests, enableIssues, enableReleases, renameMasterToMain bool
var githubDomain, githubRepo, githubToken, giteaDomain, giteaProject, giteaToken, projectsCsvPath, renameTrunkBranch, cacheFilePath string
var githubAppID, githubAppInstallationID, githubAppPrivateKeyFile string

var (
	cache             *objectCache
	errCount          int
	logger            hclog.Logger
	gh                *github.Client
	gi                *gitea.Client
	maxConcurrency    int
	getGithubGitToken func() (string, error)
	gitAuth           *dynamicGitAuth
	logWriter         = &barAwareLogWriter{}
)

// barAwareLogWriter is an io.Writer for hclog that routes log lines through
// the active MultiBar's Println so bars are redrawn after each log line.
// When no MultiBar is active it falls back to stderr directly.
type barAwareLogWriter struct {
	mu sync.Mutex
	mb *progressbar.MultiBar
}

func (w *barAwareLogWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	mb := w.mb
	w.mu.Unlock()
	if mb == nil {
		return os.Stderr.Write(p)
	}
	s := strings.TrimRight(string(p), "\n")
	if s != "" {
		mb.Println(s)
	}
	return len(p), nil
}

func (w *barAwareLogWriter) setMultiBar(mb *progressbar.MultiBar) {
	w.mu.Lock()
	w.mb = mb
	w.mu.Unlock()
}

// dynamicGitAuth implements go-git's HTTP AuthMethod interface and fetches a
// fresh token on every request, ensuring pushes survive token expiry during
// long-running rate-limit cooldowns.
type dynamicGitAuth struct {
	tokenFunc func() (string, error)
	logger    hclog.Logger
}

func (d *dynamicGitAuth) Name() string   { return "dynamic-token" }
func (d *dynamicGitAuth) String() string { return "dynamic-token" }
func (d *dynamicGitAuth) SetAuth(r *http.Request) {
	token, err := d.tokenFunc()
	if err != nil {
		d.logger.Error("failed to obtain GitHub token for git push; request will proceed unauthenticated", "error", err)
		return
	}
	r.SetBasicAuth("x-access-token", token)
}

func pushWithRetry(ctx context.Context, repo *git.Repository, opts *git.PushOptions, log hclog.Logger) error {
	var err error
	for attempt := range 3 {
		err = repo.PushContext(ctx, opts)
		isAuthErr := errors.Is(err, transport.ErrAuthenticationRequired) || errors.Is(err, transport.ErrAuthorizationFailed)
		if !isAuthErr {
			return err
		}
		if attempt < 2 {
			log.Warn("git push authentication failed, retrying", "attempt", attempt+1, "error", err)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(2 * time.Second):
			}
		}
	}
	return err
}

type Project = []string

type Report struct {
	OwnerName         string
	RepoName          string
	IssuesCount       int
	PullRequestsCount int
	ReleasesCount     int
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
		Name:   "gitea-github-migrator",
		Level:  hclog.LevelFromString(os.Getenv("LOG_LEVEL")),
		Output: logWriter,
	})

	cache = newObjectCache()

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
	flag.BoolVar(&enableReleases, "migrate-releases", false, "whether releases should be migrated (creates GitHub releases on top of mirrored tags)")
	flag.BoolVar(&renameMasterToMain, "rename-master-to-main", false, "rename master branch to main and update pull requests (incompatible with -rename-trunk-branch)")

	flag.StringVar(&githubDomain, "github-domain", constants.DefaultGithubDomain, "specifies the GitHub domain to use")
	flag.StringVar(&githubRepo, "github-repo", "", "the GitHub repository to migrate to")
	flag.StringVar(&giteaDomain, "gitea-domain", constants.DefaultGiteaDomain, "specifies the Gitea domain to use")
	flag.StringVar(&giteaProject, "gitea-project", "", "the Gitea project to migrate")
	flag.StringVar(&projectsCsvPath, "projects-csv", "", "specifies the path to a CSV file describing projects to migrate (incompatible with -gitea-project and -github-repo)")
	flag.StringVar(&renameTrunkBranch, "rename-trunk-branch", "", "specifies the new trunk branch name (incompatible with -rename-master-to-main)")

	flag.IntVar(&maxConcurrency, "max-concurrency", 4, "how many projects to migrate in parallel")
	flag.StringVar(&cacheFilePath, "cache-file", "", "path to a JSON file for persisting the cache across runs (enables faster and cheaper resumability)")

	flag.StringVar(&githubAppID, "github-app-id", "", "path to a file containing the GitHub App ID (for app-based authentication, alternative to GITHUB_TOKEN)")
	flag.StringVar(&githubAppInstallationID, "github-app-installation-id", "", "path to a file containing the GitHub App installation ID")
	flag.StringVar(&githubAppPrivateKeyFile, "github-app-private-key", "", "path to the GitHub App private key PEM file")

	flag.Parse()

	repoSpecifiedInline := githubRepo != "" && giteaProject != ""
	if (githubRepo != "" || giteaProject != "") && projectsCsvPath != "" {
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

	if maxConcurrency <= 0 {
		logger.Error("max-concurrency must be greater than 0")
		os.Exit(1)
	}

	if deleteExistingRepos && cacheFilePath != "" {
		logger.Error("cannot use -delete-existing-repos with -cache-file: the cache would be stale after repos are recreated")
		os.Exit(1)
	}

	if deleteExistingRepos && loop {
		logger.Error("cannot use -delete-existing-repos with -loop: repos would be deleted and recreated on every iteration")
		os.Exit(1)
	}

	if report {
		if loop {
			logger.Info("ignoring -loop in report mode")
		}
		if deleteExistingRepos || enableIssues || enablePullRequests || enableReleases {
			logger.Info("ignoring migration flags in report mode", "delete-existing-repos", deleteExistingRepos, "migrate-issues", enableIssues, "migrate-pull-requests", enablePullRequests, "migrate-releases", enableReleases)
		}
	}

	if cacheFilePath != "" {
		if err := cache.loadFromFile(cacheFilePath); err != nil {
			logger.Error("failed to load cache file", "path", cacheFilePath, "error", err)
			os.Exit(1)
		}
	}

	ghCfg := github_client.Config{
		Domain: githubDomain,
	}
	githubToken = os.Getenv("GITHUB_TOKEN")
	usingAppAuth := githubAppID != "" || githubAppInstallationID != "" || githubAppPrivateKeyFile != ""

	if usingAppAuth {
		if githubAppID == "" || githubAppInstallationID == "" || githubAppPrivateKeyFile == "" {
			logger.Error("must specify all of -github-app-id, -github-app-installation-id, and -github-app-private-key together")
			os.Exit(1)
		}
		appIDData, err := os.ReadFile(githubAppID)
		if err != nil {
			logger.Error("failed to read GitHub App ID file", "path", githubAppID, "error", err)
			os.Exit(1)
		}
		privateKeyBytes, err := os.ReadFile(githubAppPrivateKeyFile)
		if err != nil {
			logger.Error("failed to read GitHub App private key", "path", githubAppPrivateKeyFile, "error", err)
			os.Exit(1)
		}
		installationIDData, err := os.ReadFile(githubAppInstallationID)
		if err != nil {
			logger.Error("failed to read GitHub App installation ID file", "path", githubAppInstallationID, "error", err)
			os.Exit(1)
		}
		installationID, err := strconv.ParseInt(strings.TrimSpace(string(installationIDData)), 10, 64)
		if err != nil {
			logger.Error("invalid GitHub App installation ID in file", "path", githubAppInstallationID, "error", err)
			os.Exit(1)
		}
		ghCfg.AppID = strings.TrimSpace(string(appIDData))
		ghCfg.AppInstallationID = installationID
		ghCfg.AppPrivateKey = privateKeyBytes
	} else {
		if githubToken == "" {
			logger.Error("must set GITHUB_TOKEN or provide all GitHub App authentication flags")
			os.Exit(1)
		}
		ghCfg.Token = githubToken
	}
	if gh, getGithubGitToken, err = github_client.New(ctx, ghCfg, logger); err != nil {
		sendErr(err)
		os.Exit(1)
	}
	gitAuth = &dynamicGitAuth{tokenFunc: getGithubGitToken, logger: logger}

	giteaUrl := fmt.Sprintf("https://%s", giteaDomain)
	if gi, err = gitea.NewClient(giteaUrl, gitea.SetToken(giteaToken), gitea.SetHTTPClient(retry_client.New(logger).StandardClient()), gitea.SetContext(ctx)); err != nil {
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
		migrationErr := performMigration(ctx, projects)
		if cacheFilePath != "" {
			if err := cache.saveToFile(cacheFilePath); err != nil {
				logger.Error("failed to save cache file", "path", cacheFilePath, "error", err)
				os.Exit(1)
			}
		}
		if migrationErr != nil {
			sendErr(migrationErr)
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
	totalReleases := 0
	for _, result := range results {
		totalIssues += result.IssuesCount
		totalPullRequests += result.PullRequestsCount
		totalReleases += result.ReleasesCount
		fmt.Printf("%#v\n", result)
	}

	fmt.Println()
	fmt.Printf("Total issues: %d\n", totalIssues)
	fmt.Printf("Total pull requests: %d\n", totalPullRequests)
	fmt.Printf("Total releases: %d\n", totalReleases)
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

	var releaseCount int
	if enableReleases {
		_, releaseCount, err = entry.GetAllGiteaReleases(ctx, true)
		if err != nil {
			return nil, err
		}
	}

	return &Report{
		OwnerName:         entry.GiteaOwner,
		RepoName:          entry.GiteaRepo,
		IssuesCount:       issueCount,
		PullRequestsCount: prCount,
		ReleasesCount:     releaseCount,
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

	var mb *progressbar.MultiBar
	if !loop {
		mb = progressbar.NewMultiBar()
		logWriter.setMultiBar(mb)
		defer logWriter.setMultiBar(nil)
		defer mb.CleanUp()
	}

	for i := 0; i < concurrency; i++ {
		wg.Add(1)

		go func() {
			defer wg.Done()

			for proj := range queue {
				if err := ctx.Err(); err != nil {
					break
				}

				var bar *progressbar.Bar
				if mb != nil {
					bar = mb.NewBar(proj[0], 1)
					bar.Set(0)
				}

				if err := migrateProject(ctx, proj, bar); err != nil {
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

func migrateProject(ctx context.Context, proj []string, bar *progressbar.Bar) error {
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
		cache.purgeRepo(entry.GetCacheID())
		createRepo = true
		repoDeleted = true
	}

	// The cache file allows non-loop runs to resume after an unhandled error efficiently.
	// Without it a resume still works correctly — the GitHub Search API identifies what
	// already exists — but each already-migrated item costs a search request. On large
	// repositories this exhausts the primary rate limit before the actual resume point is
	// even reached. The cache stores that point directly, skipping the searches entirely.
	//
	// repoKnownInCache controls whether the cache is trustworthy for this repository.
	// It is only true when a cache file is in use AND the repo has entries in it, meaning a
	// previous cache-backed run already started migrating it. This is the prerequisite for
	// skipping the GitHub search on resume: items that were completed are skipped entirely;
	// items that previously failed still trigger a search to avoid duplicates.
	//
	// Scenario A – no cache file: githubLookupRequired always reflects the createRepo flag
	// (new repo → no search needed; existing repo → search to avoid duplicates).
	// Scenario B – cache file, repo is known: completed items are skipped; failed items search;
	// new items skip the search because the cache is a complete source of truth from run 1.
	// Scenario C – cache file, repo is NOT known (cache from a different run or wrong file):
	// refused with an error so the user does not silently create duplicates.
	repoKnownInCache := cacheFilePath != "" && cache.isKnownRepo(entry.GetCacheID())
	githubLookupRequired := func(index int64) bool {
		return !createRepo && (!repoKnownInCache || cache.isFailed(entry.GetCacheID(), index))
	}

	if cacheFilePath != "" && !createRepo && !repoKnownInCache && (enableIssues || enablePullRequests) {
		return fmt.Errorf("repository %s/%s already exists on GitHub but is not tracked in cache; use -delete-existing-repos to recreate it or provide a cache file from a previous run of this repository or do not use -cache-file entirely", entry.GitHubOwner, entry.GitHubRepo)
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

	entry.Logger.Debug("adding remote for GitHub repository", "url", githubUrl)
	if _, err = entry.GitRepo.CreateRemote(&config.RemoteConfig{
		Name:   "github",
		URLs:   []string{githubUrl},
		Mirror: true,
	}); err != nil {
		return fmt.Errorf("adding github remote: %v", err)
	}

	entry.Logger.Debug("force-pushing to GitHub repository", "url", githubUrl)
	if err = pushWithRetry(ctx, entry.GitRepo, &git.PushOptions{
		RemoteName: "github",
		Auth:       gitAuth,
		Force:      true,
		//Prune:      true, // causes error, attempts to delete main branch
	}, entry.Logger); err != nil {
		if errors.Is(err, git.NoErrAlreadyUpToDate) {
			entry.Logger.Debug("repository already up-to-date on GitHub", "url", githubUrl)
		} else {
			return fmt.Errorf("pushing to github repo: %v", err)
		}
	}

	entry.Logger.Debug("pushing tags to GitHub repository", "url", githubUrl)
	if err = pushWithRetry(ctx, entry.GitRepo, &git.PushOptions{
		RemoteName: "github",
		Auth:       gitAuth,
		Force:      true,
		RefSpecs:   []config.RefSpec{"refs/tags/*:refs/tags/*"},
	}, entry.Logger); err != nil {
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

	if !enableIssues && !enablePullRequests && !enableReleases {
		if bar != nil {
			bar.Set(1)
		}
		return nil
	}

	var giteaReleases []*gitea.Release
	if enableReleases {
		var err error
		giteaReleases, _, err = entry.GetAllGiteaReleases(ctx, false)
		if err != nil {
			return err
		}
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
					State: gitea.StateClosed,
				})
				phantomItemsCount++
			}
			filled = append(filled, issue)
			nextExpected = issue.Index + 1
		}
		giteaItems = filled
		if bar != nil {
			bar.Total = uint16(len(giteaItems) + len(giteaReleases))
		}

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

			cacheID := entry.GetCacheID()

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
				prHash := computePRContentHash(giteaPullRequests[idx])
				err := migratePullRequest(ctx, entry, defaultBranch, githubLookupRequired(giteaPullRequests[idx].Index), prHash, giteaPullRequests[idx])
				if err != nil {
					cache.markFailed(cacheID, giteaPullRequests[idx].Index)
					sendErr(err)
					entry.PRFailureCount++
					// fail-fast
					entry.Logger.Error("stop migration due to error to prevent ID mismatch")
					break
				}
				if entry.GitHubItemID > 0 {
					var commentEntries map[int64]migration.CommentCacheEntry
					if cached, ok := cache.getCompletedEntry(cacheID, giteaPullRequests[idx].Index); ok {
						commentEntries = cached.CommentEntries
					}
					commentEntries, err = entry.MigrateComments(ctx, giteaPullRequests[idx].Index, entry.GitHubItemID, commentEntries)
					if err != nil {
						cache.markFailed(cacheID, giteaPullRequests[idx].Index)
						sendErr(fmt.Errorf("migrating comments: %v", err))
						entry.PRFailureCount++
						// fail-fast
						entry.Logger.Error("stop migration due to error to prevent ID mismatch")
						break
					}
					cache.markCompleted(cacheID, giteaPullRequests[idx].Index, itemCacheEntry{
						ContentHash:    prHash,
						GitHubItemID:   entry.GitHubItemID,
						CommentEntries: commentEntries,
					})
					entry.PRSuccessCount++
				}
			} else {
				issueHash := computeIssueContentHash(giteaItem)
				err := migrateIssue(ctx, entry, githubLookupRequired(giteaItem.Index), issueHash, giteaItem)
				if err != nil {
					cache.markFailed(cacheID, giteaItem.Index)
					sendErr(err)
					entry.IssueFailureCount++
					// fail-fast
					entry.Logger.Error("stop migration due to error to prevent ID mismatch")
					break
				}
				if h.IsPhantomIssue(giteaItem) {
					cache.markCompleted(cacheID, giteaItem.Index, itemCacheEntry{
						ContentHash:  issueHash,
						GitHubItemID: entry.GitHubItemID,
					})
					entry.IssueSuccessCount++
					continue
				}
				// always migrate comments to update if necessary
				var commentEntries map[int64]migration.CommentCacheEntry
				if cached, ok := cache.getCompletedEntry(cacheID, giteaItem.Index); ok {
					commentEntries = cached.CommentEntries
				}
				commentEntries, err = entry.MigrateComments(ctx, giteaItem.Index, entry.GitHubItemID, commentEntries)
				if err != nil {
					cache.markFailed(cacheID, giteaItem.Index)
					sendErr(fmt.Errorf("migrating comments: %v", err))
					entry.IssueFailureCount++
					// fail-fast
					entry.Logger.Error("stop migration due to error to prevent ID mismatch")
					break
				}
				cache.markCompleted(cacheID, giteaItem.Index, itemCacheEntry{
					ContentHash:    issueHash,
					GitHubItemID:   entry.GitHubItemID,
					CommentEntries: commentEntries,
				})
				entry.IssueSuccessCount++
			}
			if bar != nil {
				bar.Inc()
			}
		}

		issueSkipped := totalCount + phantomItemsCount - prTotalCount - entry.IssueSuccessCount - entry.IssueFailureCount
		prSkipped := prTotalCount - entry.PRSuccessCount - entry.PRFailureCount
		entry.Logger.Info("migrated issues from Gitea to GitHub", "successful", entry.IssueSuccessCount, "failed", entry.IssueFailureCount, "skipped", issueSkipped)
		entry.Logger.Info("migrated pull requests from Gitea to GitHub", "successful", entry.PRSuccessCount, "failed", entry.PRFailureCount, "skipped", prSkipped)
	} else {
		cacheID := entry.GetCacheID()
		if enableIssues {
			giteaIssues, totalCount, err := entry.GetAllGiteaIssues(ctx, gitea.IssueTypeIssue, false)
			if err != nil {
				return err
			}

			entry.Logger.Info("migrating issues from Gitea to GitHub", "count", totalCount)
			if bar != nil {
				bar.Total = uint16(len(giteaIssues) + len(giteaReleases))
			}
			for _, giteaIssue := range giteaIssues {
				if giteaIssue == nil {
					continue
				}

				if err := ctx.Err(); err != nil {
					sendErr(fmt.Errorf("preparing to migrate issue: %v", err))
					break
				}

				issueHash := computeIssueContentHash(giteaIssue)
				err := migrateIssue(ctx, entry, githubLookupRequired(giteaIssue.Index), issueHash, giteaIssue)
				if err != nil {
					cache.markFailed(cacheID, giteaIssue.Index)
					sendErr(err)
					entry.IssueFailureCount++
				} else {
					var commentEntries map[int64]migration.CommentCacheEntry
					if cached, ok := cache.getCompletedEntry(cacheID, giteaIssue.Index); ok {
						commentEntries = cached.CommentEntries
					}
					commentEntries, err = entry.MigrateComments(ctx, giteaIssue.Index, entry.GitHubItemID, commentEntries)
					if err != nil {
						cache.markFailed(cacheID, giteaIssue.Index)
						sendErr(fmt.Errorf("migrating comments: %v", err))
						entry.IssueFailureCount++
					} else {
						cache.markCompleted(cacheID, giteaIssue.Index, itemCacheEntry{
							ContentHash:    issueHash,
							GitHubItemID:   entry.GitHubItemID,
							CommentEntries: commentEntries,
						})
						entry.IssueSuccessCount++
					}
				}
				if bar != nil {
					bar.Inc()
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
			if bar != nil {
				bar.Total = uint16(len(giteaPullRequests) + len(giteaReleases))
			}
			for _, giteaPullRequest := range giteaPullRequests {
				if giteaPullRequest == nil {
					continue
				}

				// Check for context cancellation
				if err := ctx.Err(); err != nil {
					sendErr(fmt.Errorf("preparing to list pull requests: %v", err))
					break
				}

				prHash := computePRContentHash(giteaPullRequest)
				err := migratePullRequest(ctx, entry, defaultBranch, githubLookupRequired(giteaPullRequest.Index), prHash, giteaPullRequest)
				if err != nil {
					cache.markFailed(cacheID, giteaPullRequest.Index)
					sendErr(err)
					entry.PRFailureCount++
				} else {
					var commentEntries map[int64]migration.CommentCacheEntry
					if cached, ok := cache.getCompletedEntry(cacheID, giteaPullRequest.Index); ok {
						commentEntries = cached.CommentEntries
					}
					commentEntries, err = entry.MigrateComments(ctx, giteaPullRequest.Index, entry.GitHubItemID, commentEntries)
					if err != nil {
						cache.markFailed(cacheID, giteaPullRequest.Index)
						sendErr(fmt.Errorf("migrating comments: %v", err))
						entry.PRFailureCount++
					} else {
						cache.markCompleted(cacheID, giteaPullRequest.Index, itemCacheEntry{
							ContentHash:    prHash,
							GitHubItemID:   entry.GitHubItemID,
							CommentEntries: commentEntries,
						})
						entry.PRSuccessCount++
					}
				}
				if bar != nil {
					bar.Inc()
				}
			}

			skippedCount := totalCount - entry.PRSuccessCount - entry.PRFailureCount
			entry.Logger.Info("migrated pull requests from Gitea to GitHub", "successful", entry.PRSuccessCount, "failed", entry.PRFailureCount, "skipped", skippedCount)
		}
	}

	if enableReleases {
		if bar != nil && !enableIssues && !enablePullRequests {
			bar.Total = uint16(len(giteaReleases))
		}

		entry.Logger.Info("migrating releases from Gitea to GitHub", "count", len(giteaReleases))
		cacheID := entry.GetCacheID()
		for _, giteaRelease := range giteaReleases {
			if giteaRelease == nil {
				continue
			}

			// Check for context cancellation
			if err := ctx.Err(); err != nil {
				sendErr(fmt.Errorf("preparing to migrate release: %v", err))
				break
			}

			err := migrateRelease(ctx, entry, giteaRelease)
			if err != nil {
				cache.markReleaseFailed(cacheID, giteaRelease.ID)
				sendErr(fmt.Errorf("migrating release %s: %v", giteaRelease.TagName, err))
				entry.ReleaseFailureCount++
			} else {
				cache.markReleaseCompleted(cacheID, giteaRelease.ID, releaseCacheEntry{
					ContentHash:     computeReleaseContentHash(giteaRelease),
					GitHubReleaseID: entry.GitHubReleaseID,
				})
				entry.ReleaseSuccessCount++
			}
			if bar != nil {
				bar.Inc()
			}
		}

		skippedCount := len(giteaReleases) - entry.ReleaseSuccessCount - entry.ReleaseFailureCount
		entry.Logger.Info("migrated releases from Gitea to GitHub", "successful", entry.ReleaseSuccessCount, "failed", entry.ReleaseFailureCount, "skipped", skippedCount)
	}

	return nil
}

func computePRContentHash(pr *gitea.PullRequest) string {
	var mergedAt, closedAt string
	if pr.Merged != nil {
		mergedAt = pr.Merged.Format(time.RFC3339)
	}
	if pr.Closed != nil {
		closedAt = pr.Closed.Format(time.RFC3339)
	}
	s := md5.Sum([]byte(fmt.Sprintf("%s\x00%s\x00%s\x00%v\x00%s\x00%s\x00%s\x00%s\x00%d",
		pr.Title, pr.Body, string(pr.State), pr.HasMerged, mergedAt, closedAt, pr.Base.Ref, pr.MergeBase, pr.Comments)))
	return hex.EncodeToString(s[:])
}

func computeIssueContentHash(issue *gitea.Issue) string {
	var closedAt string
	if issue.Closed != nil {
		closedAt = issue.Closed.Format(time.RFC3339)
	}
	s := md5.Sum([]byte(fmt.Sprintf("%s\x00%s\x00%s\x00%s\x00%d",
		issue.Title, issue.Body, string(issue.State), closedAt, issue.Comments)))
	return hex.EncodeToString(s[:])
}

func migratePullRequest(ctx context.Context, entry *migration.Entry, defaultBranch string, githubLookupRequired bool, contentHash string, giteaPullRequest *gitea.PullRequest) error {
	entry.GitHubItemID = 0
	var resumeEntry *itemCacheEntry
	if cached, ok := cache.getCompletedEntry(entry.GetCacheID(), giteaPullRequest.Index); ok {
		if cached.ContentHash == contentHash {
			entry.Logger.Debug("skipping unchanged pull request (hash match)", "pr_number", giteaPullRequest.Index)
			entry.GitHubItemID = cached.GitHubItemID
			return nil
		}
		resumeEntry = &cached
	}

	if giteaPullRequest.MergeBase == "" {
		return fmt.Errorf("identifying suitable merge base for pull request %d", giteaPullRequest.Index)
	}

	sourceBranchForClosedOrOpenForkPullRequest := fmt.Sprintf("migration-source-%d/%s", giteaPullRequest.Index, giteaPullRequest.Head.Ref)
	targetBranchForClosedPullRequest := fmt.Sprintf("migration-target-%d/%s", giteaPullRequest.Index, giteaPullRequest.Base.Ref)

	isForkPR := giteaPullRequest.Head.Repository == nil || giteaPullRequest.Head.Repository.ID != entry.GiteaRepository.ID
	isOpenForkPR := isForkPR && strings.EqualFold(string(giteaPullRequest.State), string(gitea.StateOpen))

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

	if resumeEntry != nil {
		entry.Logger.Debug("fetching existing pull request by cached number", "pr_number", giteaPullRequest.Index, "github_number", resumeEntry.GitHubItemID)
		ghPr, err := getGithubPullRequest(ctx, entry.GitHubOwner, entry.GitHubRepo, int(resumeEntry.GitHubItemID))
		if err != nil {
			return fmt.Errorf("retrieving pull request: %v", err)
		}
		githubPullRequest = ghPr
	} else if githubLookupRequired {
		entry.Logger.Debug("searching for any existing pull request", "pr_number", giteaPullRequest.Index)
		sourceBranches := []string{giteaPullRequest.Head.Ref, sourceBranchForClosedOrOpenForkPullRequest}
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

	if githubPullRequest == nil {
		if isOpenForkPR {
			entry.Logger.Trace("checkout source branch for open pull request from fork", "repository_id", entry.GiteaRepository.ID, "pr_number", giteaPullRequest.Index)
			if err = worktree.Checkout(&git.CheckoutOptions{
				Create: true,
				Force:  true,
				Branch: plumbing.NewBranchReferenceName(sourceBranchForClosedOrOpenForkPullRequest),
				Hash:   h.Conditional(tmpEmptyCommitRequired, plumbing.ZeroHash, prHeadHash),
			}); err != nil {
				return fmt.Errorf("checking out source branch for open pull request from fork #%d: %v", giteaPullRequest.Index, err)
			}

			entry.Logger.Trace("pushing source branch for open pull request from fork", "repository_id", entry.GiteaRepository.ID, "pr_number", giteaPullRequest.Index)

			if err = pushWithRetry(ctx, entry.GitRepo, &git.PushOptions{
				RemoteName: "github",
				Auth:       gitAuth,
				RefSpecs: []config.RefSpec{
					config.RefSpec(fmt.Sprintf("refs/heads/%[1]s:refs/heads/%[1]s", sourceBranchForClosedOrOpenForkPullRequest)),
				},
				Force: true,
			}, entry.Logger); err != nil {
				if errors.Is(err, git.NoErrAlreadyUpToDate) {
					entry.Logger.Trace("open fork PR branch already exists and is up-to-date on GitHub", "source_branch", giteaPullRequest.Head.Ref, "target_branch", giteaPullRequest.Base.Ref)
				} else {
					return fmt.Errorf("pushing open fork PR branch to github: %v", err)
				}
			}

			giteaPullRequest.Head.Ref = sourceBranchForClosedOrOpenForkPullRequest

			if tmpEmptyCommitRequired {
				// open PRs from forks that have zero commits will auto-closed on GitHub, so the source branch can be deleted.
				cleanUpBranch = true
			}
		}

		if tmpEmptyCommitRequired {
			entry.Logger.Trace("checkout source branch for empty commit list pull request", "repository_id", entry.GiteaRepository.ID, "pr_number", giteaPullRequest.Index)
			if err = worktree.Checkout(&git.CheckoutOptions{
				Create: h.Conditional(giteaPullRequest.State == gitea.StateOpen, false, true),
				Force:  true,
				Branch: plumbing.NewBranchReferenceName(h.Conditional(giteaPullRequest.State == gitea.StateClosed, sourceBranchForClosedOrOpenForkPullRequest, giteaPullRequest.Head.Ref)),
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

				if err = pushWithRetry(ctx, entry.GitRepo, &git.PushOptions{
					RemoteName: "github",
					Auth:       gitAuth,
					RefSpecs: []config.RefSpec{
						config.RefSpec(fmt.Sprintf("refs/heads/%[1]s:refs/heads/%[1]s", giteaPullRequest.Head.Ref)),
					},
					Force: true,
				}, entry.Logger); err != nil {
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
			giteaPullRequest.Head.Ref = sourceBranchForClosedOrOpenForkPullRequest
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
			if err = pushWithRetry(ctx, entry.GitRepo, &git.PushOptions{
				RemoteName: "github",
				Auth:       gitAuth,
				RefSpecs: []config.RefSpec{
					config.RefSpec(fmt.Sprintf("refs/heads/%[1]s:refs/heads/%[1]s", giteaPullRequest.Head.Ref)),
					config.RefSpec(fmt.Sprintf("refs/heads/%[1]s:refs/heads/%[1]s", giteaPullRequest.Base.Ref)),
				},
				Force: true,
			}, entry.Logger); err != nil {
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
		if err := h.SleepWithContext(ctx, constants.GithubApiPauseBetweenMutativeRequests); err != nil {
			return err
		}

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

			if err = pushWithRetry(ctx, entry.GitRepo, &git.PushOptions{
				RemoteName: "github",
				Auth:       gitAuth,
				RefSpecs: []config.RefSpec{
					config.RefSpec(fmt.Sprintf("refs/heads/%[1]s:refs/heads/%[1]s", giteaPullRequest.Head.Ref)),
				},
				Force: true,
			}, entry.Logger); err != nil {
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
			if err := h.SleepWithContext(ctx, constants.GithubApiPauseBetweenMutativeRequests); err != nil {
				return err
			}

			entry.Logger.Debug("creating empty commit list auto-close comment", "pr_number", giteaPullRequest.Index)
			newComment := github.IssueComment{
				Body: h.Pointer(`> [!CAUTION]
>
> **Due to platform limitations in handling PRs with empty commit history or orphaned commits, this PR was flagged as "merged" or "closed". This does not necessarily represent its original state within Gitea.**`),
			}
			if _, _, err = gh.Issues.CreateComment(ctx, entry.GitHubOwner, entry.GitHubRepo, githubPullRequest.GetNumber(), &newComment); err != nil {
				return fmt.Errorf("creating empty commit list auto-close comment: %v", err)
			}
			if err := h.SleepWithContext(ctx, constants.GithubApiPauseBetweenMutativeRequests); err != nil {
				return err
			}
		}

		if giteaPullRequest.State == gitea.StateClosed {
			entry.Logger.Debug("closing pull request", "pr_number", githubPullRequest.GetNumber())

			githubPullRequest.State = h.Pointer("closed")
			if githubPullRequest, _, err = gh.PullRequests.Edit(ctx, entry.GitHubOwner, entry.GitHubRepo, githubPullRequest.GetNumber(), githubPullRequest); err != nil {
				return fmt.Errorf("updating pull request: %v", err)
			}
			if err := h.SleepWithContext(ctx, constants.GithubApiPauseBetweenMutativeRequests); err != nil {
				return err
			}
		}

	} else {
		var newState *string
		switch giteaPullRequest.State {
		case "open":
			newState = h.Pointer("open")
		case "closed", "merged":
			newState = h.Pointer("closed")
		}

		// PRs with empty commit lists or orphaned commits are auto-closed by GitHub and cannot be reopened.
		if tmpEmptyCommitRequired && newState != nil && *newState == "open" {
			entry.Logger.Debug("keeping PR closed - auto-closed by GitHub due to empty commit list or orphaned commit, cannot reopen", "pr_number", githubPullRequest.GetNumber())
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
			if err := h.SleepWithContext(ctx, constants.GithubApiPauseBetweenMutativeRequests); err != nil {
				return err
			}
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
			if err := h.SleepWithContext(ctx, constants.GithubApiPauseBetweenMutativeRequests); err != nil {
				return err
			}
		} else {
			entry.Logger.Trace("existing pull request is up-to-date", "pr_number", githubPullRequest.GetNumber())
		}
	}

	if cleanUpBranch {
		var refSpec []config.RefSpec
		if isOpenForkPR && tmpEmptyCommitRequired {
			entry.Logger.Debug("deleting temporary source branch for open fork PR with empty commit list", "pr_number", githubPullRequest.GetNumber(), "source_branch", giteaPullRequest.Head.Ref, "target_branch", giteaPullRequest.Base.Ref)
			refSpec = []config.RefSpec{
				config.RefSpec(fmt.Sprintf(":refs/heads/%s", giteaPullRequest.Head.Ref)),
			}
		} else {
			entry.Logger.Debug("deleting temporary branches for closed pull request", "pr_number", githubPullRequest.GetNumber(), "source_branch", giteaPullRequest.Head.Ref, "target_branch", giteaPullRequest.Base.Ref)
			refSpec = []config.RefSpec{
				config.RefSpec(fmt.Sprintf(":refs/heads/%s", giteaPullRequest.Head.Ref)),
				config.RefSpec(fmt.Sprintf(":refs/heads/%s", giteaPullRequest.Base.Ref)),
			}
		}
		if err = pushWithRetry(ctx, entry.GitRepo, &git.PushOptions{
			RemoteName: "github",
			Auth:       gitAuth,
			RefSpecs:   refSpec,
			Force:      true,
		}, entry.Logger); err != nil {
			if errors.Is(err, git.NoErrAlreadyUpToDate) {
				entry.Logger.Trace("branches already deleted on GitHub", "pr_number", githubPullRequest.GetNumber(), "source_branch", giteaPullRequest.Head.Ref, "target_branch", giteaPullRequest.Base.Ref)
			} else {
				return fmt.Errorf("pushing branch deletions to github: %v", err)
			}
		}
	}

	entry.GitHubItemID = int64(githubPullRequest.GetNumber())
	return nil
}

func migrateIssue(ctx context.Context, entry *migration.Entry, githubLookupRequired bool, contentHash string, giteaIssue *gitea.Issue) error {
	entry.GitHubItemID = 0
	cacheID := entry.GetCacheID()

	// Phantom guard: handle before any content hash work
	// It might be a Github-side existing PR that was later deleted on Gitea-side
	if h.IsPhantomIssue(giteaIssue) {
		if cached, ok := cache.getCompletedEntry(cacheID, giteaIssue.Index); ok {
			entry.Logger.Debug("skipping already completed phantom issue", "issue_number", giteaIssue.Index)
			entry.GitHubItemID = cached.GitHubItemID
			return nil
		}
	}

	var resumeEntry *itemCacheEntry
	if !h.IsPhantomIssue(giteaIssue) {
		if cached, ok := cache.getCompletedEntry(cacheID, giteaIssue.Index); ok {
			if cached.ContentHash == contentHash {
				entry.Logger.Debug("skipping unchanged issue (hash match)", "issue_number", giteaIssue.Index)
				entry.GitHubItemID = cached.GitHubItemID
				return nil
			}
			resumeEntry = &cached
		}
	}

	var githubIssue *github.Issue

	if resumeEntry != nil {
		entry.Logger.Debug("fetching existing issue by cached number", "issue_number", giteaIssue.Index, "github_number", resumeEntry.GitHubItemID)
		result, _, err := gh.Issues.Get(ctx, entry.GitHubOwner, entry.GitHubRepo, int(resumeEntry.GitHubItemID))
		if err != nil {
			return fmt.Errorf("retrieving issue: %v", err)
		}
		githubIssue = result
	} else if githubLookupRequired {
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
	if h.IsPhantomIssue(giteaIssue) {
		originalState = "> This issue does not exist on Gitea. It was created to fill a gap in issue/PR ID list."
	} else if giteaIssue.State == gitea.StateClosed {
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
		if err := h.SleepWithContext(ctx, constants.GithubApiPauseBetweenMutativeRequests); err != nil {
			return err
		}

		if giteaIssue.State == gitea.StateClosed {
			entry.Logger.Debug("closing issue", "issue_number", githubIssue.GetNumber())
			if _, _, err = gh.Issues.Edit(ctx, entry.GitHubOwner, entry.GitHubRepo, githubIssue.GetNumber(), &github.IssueRequest{State: h.Pointer("closed")}); err != nil {
				return fmt.Errorf("closing issue: %v", err)
			}
			if err := h.SleepWithContext(ctx, constants.GithubApiPauseBetweenMutativeRequests); err != nil {
				return err
			}
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
			if err := h.SleepWithContext(ctx, constants.GithubApiPauseBetweenMutativeRequests); err != nil {
				return err
			}
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
			if err := h.SleepWithContext(ctx, constants.GithubApiPauseBetweenMutativeRequests); err != nil {
				return err
			}
		} else {
			entry.Logger.Trace("existing issue is up-to-date", "issue_number", githubIssue.GetNumber())
		}
	}

	entry.GitHubItemID = int64(githubIssue.GetNumber())
	return nil
}

func computeReleaseContentHash(release *gitea.Release) string {
	s := md5.Sum([]byte(fmt.Sprintf("%s\x00%s\x00%v\x00%v\x00%s",
		release.Title, release.Note, release.IsDraft, release.IsPrerelease, release.CreatedAt.Format(time.RFC3339))))
	return hex.EncodeToString(s[:])
}

func migrateRelease(ctx context.Context, entry *migration.Entry, giteaRelease *gitea.Release) error {
	entry.GitHubReleaseID = 0
	cacheID := entry.GetCacheID()

	// Draft releases may reference a tag that doesn't exist yet — skip the check for those.
	if !giteaRelease.IsDraft {
		if _, err := entry.GitRepo.Reference(plumbing.NewTagReferenceName(giteaRelease.TagName), false); err != nil {
			return fmt.Errorf("tag %q not found in local repository: %v", giteaRelease.TagName, err)
		}
	}

	contentHash := computeReleaseContentHash(giteaRelease)

	var resumeEntry *releaseCacheEntry
	if cached, ok := cache.getCompletedReleaseEntry(cacheID, giteaRelease.ID); ok {
		if cached.ContentHash == contentHash {
			entry.Logger.Debug("skipping unchanged release (hash match)", "tag", giteaRelease.TagName)
			entry.GitHubReleaseID = cached.GitHubReleaseID
			return nil
		}
		resumeEntry = &cached
	}

	note := giteaRelease.Note
	if strings.TrimSpace(note) == "" {
		note = "_No description_"
	}
	draftLabel := "No"
	if giteaRelease.IsDraft {
		draftLabel = "Yes"
	}
	prereleaseLabel := "No"
	if giteaRelease.IsPrerelease {
		prereleaseLabel = "Yes"
	}

	body := fmt.Sprintf(`> [!NOTE]
> This release was migrated from Gitea
>
> |      |      |
> | ---- | ---- |
> | **Original Author** | %[1]s |
> | **Tag Name** | %[2]s |
> | **Date Originally Created** | %[3]s |
> | **Date Originally Published** | %[4]s |
> | **Draft** | %[5]s |
> | **Pre-release** | %[6]s |
> | **Target Commitish** | %[7]s |
> |      |      |

## Original Description

%[8]s`,
		h.GetGitHubAccountReference(giteaRelease.Publisher),
		giteaRelease.TagName,
		giteaRelease.CreatedAt.Format(constants.DateFormat),
		giteaRelease.PublishedAt.Format(constants.DateFormat),
		draftLabel,
		prereleaseLabel,
		giteaRelease.Target,
		note)

	if len(body) > constants.GithubBodyLimit {
		entry.Logger.Warn("release body was truncated due to platform limits", "tag", giteaRelease.TagName)
		body = body[:constants.GithubBodyLimit] + "..."
	}

	var githubRelease *github.RepositoryRelease

	if resumeEntry != nil {
		entry.Logger.Debug("fetching existing release by cached ID", "tag", giteaRelease.TagName, "github_release_id", resumeEntry.GitHubReleaseID)
		r, _, err := gh.Repositories.GetRelease(ctx, entry.GitHubOwner, entry.GitHubRepo, resumeEntry.GitHubReleaseID)
		if err != nil {
			return fmt.Errorf("retrieving release: %v", err)
		}
		githubRelease = r
	} else {
		if giteaRelease.IsDraft {
			// GetReleaseByTag does not return draft releases; list all releases and match by tag name.
			entry.Logger.Debug("looking up existing draft release by tag name", "tag", giteaRelease.TagName)
			opts := &github.ListOptions{PerPage: 100}
			for {
				releases, resp, err := gh.Repositories.ListReleases(ctx, entry.GitHubOwner, entry.GitHubRepo, opts)
				if err != nil {
					return fmt.Errorf("listing releases to find draft: %v", err)
				}
				for _, r := range releases {
					if r.GetTagName() == giteaRelease.TagName {
						githubRelease = r
						break
					}
				}
				if githubRelease != nil || resp.NextPage == 0 {
					break
				}
				opts.Page = resp.NextPage
			}
		} else {
			entry.Logger.Debug("looking up existing release by tag", "tag", giteaRelease.TagName)
			r, resp, err := gh.Repositories.GetReleaseByTag(ctx, entry.GitHubOwner, entry.GitHubRepo, giteaRelease.TagName)
			if err != nil && (resp == nil || resp.StatusCode != http.StatusNotFound) {
				return fmt.Errorf("looking up release by tag: %v", err)
			}
			if err == nil {
				githubRelease = r
			}
		}
	}

	if githubRelease == nil {
		entry.Logger.Info("creating release", "tag", giteaRelease.TagName)
		r, _, err := gh.Repositories.CreateRelease(ctx, entry.GitHubOwner, entry.GitHubRepo, &github.RepositoryRelease{
			TagName:    h.Pointer(giteaRelease.TagName),
			Name:       h.Pointer(giteaRelease.Title),
			Body:       h.Pointer(body),
			Draft:      h.Pointer(giteaRelease.IsDraft),
			Prerelease: h.Pointer(giteaRelease.IsPrerelease),
		})
		if err != nil {
			return fmt.Errorf("creating release: %v", err)
		}
		if err := h.SleepWithContext(ctx, constants.GithubApiPauseBetweenMutativeRequests); err != nil {
			return err
		}
		githubRelease = r
	} else {
		needsUpdate := (githubRelease.Name == nil || *githubRelease.Name != giteaRelease.Title) ||
			(githubRelease.Body == nil || *githubRelease.Body != body) ||
			(githubRelease.Draft == nil || *githubRelease.Draft != giteaRelease.IsDraft) ||
			(githubRelease.Prerelease == nil || *githubRelease.Prerelease != giteaRelease.IsPrerelease)

		if needsUpdate {
			entry.Logger.Info("updating release", "tag", giteaRelease.TagName)
			r, _, err := gh.Repositories.EditRelease(ctx, entry.GitHubOwner, entry.GitHubRepo, githubRelease.GetID(), &github.RepositoryRelease{
				TagName:    h.Pointer(giteaRelease.TagName),
				Name:       h.Pointer(giteaRelease.Title),
				Body:       h.Pointer(body),
				Draft:      h.Pointer(giteaRelease.IsDraft),
				Prerelease: h.Pointer(giteaRelease.IsPrerelease),
			})
			if err != nil {
				return fmt.Errorf("updating release: %v", err)
			}
			if err := h.SleepWithContext(ctx, constants.GithubApiPauseBetweenMutativeRequests); err != nil {
				return err
			}
			githubRelease = r
		} else {
			entry.Logger.Trace("existing release is up-to-date", "tag", giteaRelease.TagName)
		}
	}

	entry.GitHubReleaseID = githubRelease.GetID()
	return nil
}
