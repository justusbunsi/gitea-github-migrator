# Gitea to GitHub Repository Migration Tool

> Many thanks to @manicminer for their excellent work on the [GitLab to GitHub migration tool](https://github.com/manicminer/gitlab-migrator).
> This repository is based on that repository and adapted to support Gitea to GitHub migrations.

This tool can migrate projects from Gitea to repositories on GitHub. It currently supports:

* migrating the git repository with full history
* migrating pull requests and translating them into pull requests, including closed/merged ones
* migrating issues, including closed ones
* migrating releases (including draft and pre-release)
* renaming the `master` branch to `main` along the way

It does not support migrating wikis or any other primitive at this time. PRs welcome! (Although please don't waste your time suggesting swathing changes by an LLM)

Both gitea.com and Gitea self-hosted are supported, as well as github.com and GitHub Enterprise (latter untested).

## Installing

Download the [latest release](https://github.com/justusbunsi/gitea-github-migrator/releases/latest) for your platform & architecture. Alternatively,
```
go install github.com/justusbunsi/gitea-github-migrator
```

or download the most appropriate binary for your OS & platform from the [latest release](https://github.com/justusbunsi/gitea-github-migrator/releases/latest).

Golang 1.23 was used, you may have luck with earlier releases.

## Usage

_Example Usage_

```
gitea-github-migrator -gitea-project=mygiteauser/myproject -github-repo=mygithubuser/myrepo -migrate-pull-requests -migrate-issues
```

Written in Go, this is a cross-platform CLI utility that accepts the following runtime arguments:

```
  -delete-existing-repos
        whether existing repositories should be deleted before migrating
  -github-domain string
        specifies the GitHub domain to use (default "github.com")
  -github-repo string
        the GitHub repository to migrate to
  -gitea-domain string
        specifies the Gitea domain to use (default "gitea.com")
  -gitea-project string
        the Gitea project to migrate
  -loop
        continue migrating until canceled
  -max-concurrency int
        how many projects to migrate in parallel (default 4)
  -migrate-pull-requests
        whether pull requests should be migrated
  -migrate-issues
        whether issues should be migrated
  -migrate-releases
        whether releases should be migrated (creates GitHub releases on top of mirrored tags)
  -projects-csv string
        specifies the path to a CSV file describing projects to migrate (incompatible with -gitea-project and -github-repo)
  -rename-master-to-main
        rename master branch to main and update pull requests (incompatible with -rename-trunk-branch)
  -rename-trunk-branch string
        specifies the new trunk branch name (incompatible with -rename-master-to-main)
  -report
        report on primitives to be migrated instead of beginning migration
  -cache-file string
        The cache file allows non-loop runs to resume after an unhandled error efficiently.
        Without it a resume still works correctly - the GitHub Search API identifies what
        already exists - but each already-migrated item costs a search request. On large
        repositories this exhausts the primary rate limit before the actual resume point is
        even reached. The cache stores that point directly, skipping the searches entirely.
  -github-app-id string
        path to a file containing the GitHub App ID (for app-based authentication, alternative to GITHUB_TOKEN)
  -github-app-installation-id string
        path to a file containing the GitHub App installation ID (see https://docs.github.com/en/apps/using-github-apps/installing-a-github-app-from-a-third-party)
  -github-app-private-key string
        path to the GitHub App private key PEM file (see https://docs.github.com/en/apps/creating-github-apps/authenticating-with-a-github-app/managing-private-keys-for-github-apps)
```

You can specify an individual Gitea project with the `-gitea-project` argument, along with the target GitHub repository with the `-github-repo` argument.

Alternatively, you can supply the path to a CSV file with the `-projects-csv` argument, which should contain two columns:

```csv
gitea-org-or-user/gitea-project-name,github-org-or-user/github-repo-name
```

For authentication, the `GITEA_TOKEN` environment variable must always be populated. For GitHub authentication, one of the following must be provided:

- **Personal Access Token**: set the `GITHUB_TOKEN` environment variable. You cannot specify tokens as command-line arguments.
- **GitHub App**: specify `-github-app-id`, `-github-app-installation-id`, and `-github-app-private-key` together. All three are required when using app-based authentication. Each flag accepts a path to a file whose contents are read and trimmed of surrounding whitespace - this avoids exposing sensitive values in shell history and works naturally with Docker secrets and Kubernetes secret mounts. Installation tokens are short-lived but refreshed automatically - no special handling is needed. The app must be installed on the target GitHub organization or user account and granted sufficient permissions (contents, pull requests, issues).

To enable migration of Gitea pull requests to GitHub pull requests (including closed/merged ones!), specify `-migrate-pull-requests`.

To enable migration of Gitea issues to GitHub issues (including closed ones!), specify `-migrate-issues`.

To enable migration of Gitea releases to GitHub releases (including drafts and pre-releases!), specify `-migrate-releases`. Releases require the corresponding tags to exist in the mirrored git repository — this tool verifies tag presence locally before attempting creation. If a tag is missing, the release is skipped and counted as a failure. When used together with `-cache-file`, releases are tracked under a separate `completed_releases` / `failed_releases` section in the cache file, keyed by Gitea release ID.

Note: If `-migrate-pull-requests` and `-migrate-issues` are both enabled, the issue/pull ID order will be preserved on GitHub. On errors creating issues/pulls, the tool stops migrating that repository to prevent ID mismatches. In case of deleted issues/pulls in Gitea, a phantom issue is created to allocate the ID.

To delete existing GitHub repos prior to migrating, pass the `-delete-existing-repos` argument. _This is potentially dangerous, you won't be asked for confirmation._

Note: If the destination repository does not exist, this tool will attempt to create a private repository. If the destination repo already exists, it will be used unless you specify `-delete-existing-repos`

Specify the location of a self-hosted instance of Gitea with the `-gitea-domain` argument, or a GitHub Enterprise instance with the `-github-domain` argument.

As a bonus, this tool can transparently rename the trunk branch on your GitHub repository - enable with the `-rename-trunk-branch` argument. This will also work for any open pull requests as they are translated to pull requests.

By default, 4 workers will be spawned to migrate up to 4 projects in parallel. You can increase or decrease this with the `-max-concurrency` argument. Note that due to GitHub API rate-limiting, you may not experience any significant speed-up. See [GitHub API docs](https://docs.github.com/en/rest/using-the-rest-api/rate-limits-for-the-rest-api) for details.

Specify `-loop` to continue migrating projects until canceled. This is useful for daemonizing the migration tool, or automatically restarting when migrating a large number of projects (or a small number of very large projects).

## Logging

This tool is entirely noninteractive and outputs different levels of logs depending on your interest. You can set the `LOG_LEVEL` environment to one of `ERROR`, `WARN`, `INFO`, `DEBUG` or `TRACE` to get more or less verbosity. The default is `INFO`.

## Caching

The tool maintains a thread-safe in-memory cache for certain primitives, in order to help reduce the number of API requests being made. At this time, the following are cached the first time they are encountered, and thereafter retrieved from the cache until the tool is restarted:

- GitHub pull requests
- GitHub issue search results
- GitHub user profiles
- Gitea user profiles

## Idempotence

This tool tries to be idempotent. You can run it over and over and it will patch the GitHub repository, along with its pull requests and issues, to match what you have in Gitea. This should help you migrate a number of projects without enacting a large maintenance window.

_Note that this tool performs a forced mirror push, so it's not recommended to run this tool after commencing work in the target repository._

For pull requests, issues and their comments, the corresponding IDs from Gitea are added to the Markdown header, this is parsed to enable idempotence (see next section).

## Pull Requests

Whilst the git repository will be migrated verbatim, the pull requests are managed using the GitHub API and typically will be authored by the person supplying the authentication token.

Each pull request, along with every comment, will be prepended with a Markdown table showing the original author and some other metadata that is useful to know.  This is also used to map pull requests and their comments to their counterparts in Gitea and enables the tool to be idempotent.

As a bonus, if your Gitea users add the URL to their GitHub profile in the `Website` field of their Gitea profile, this tool will add a link to their GitHub profile in the markdown header of any PR or comment they originally authored.

This tool also migrates merged/closed pull requests from your Gitea projects. It does this by reconstructing temporary branches in each repo, pushing them to GitHub, creating then closing the pull request, and lastly deleting the temporary branches. Once the tool has completed, you should not have any of these temporary branches in your repo - although GitHub will not garbage collect them immediately such that you can click the `Restore branch` button in any of these PRs.

_Example migrated pull request (open)_

![example migrated open pull request](pr-example-open.jpeg)

_Example migrated pull request (closed)_

![example migrated closed pull request](pr-example-closed.jpeg)

## Releases

Whilst the git repository (including tags) will be migrated verbatim, releases are managed using the GitHub API and will be authored by the person supplying the authentication token.

Each release body will be prepended with a Markdown table showing the original author and relevant metadata: tag name, creation date, publication date, draft and pre-release status, and target commitish. The original release description follows in a separate section.

Release migration is idempotent: each run looks up the existing GitHub release by tag name (or by cached release ID when a `-cache-file` is in use) and updates it only when the content has changed. Changes are detected via an MD5 hash of the title, body, draft flag, pre-release flag, and creation date.

Asset files attached to Gitea releases are not migrated.

## Issues

Whilst the git repository will be migrated verbatim, the issues are managed using the GitHub API and typically will be authored by the person supplying the authentication token.

Each issue, along with every comment, will be prepended with a Markdown table showing the original author and some other metadata that is useful to know. This is also used to map issues and their comments to their counterparts in Gitea and enables the tool to be idempotent.

As a bonus, if your Gitea users add the URL to their GitHub profile in the `Website` field of their Gitea profile, this tool will add a link to their GitHub profile in the markdown header of any issue or comment they originally authored.

The design of migrated description and comments is similar to the Pull Request design. See previous section.

## Contributing, reporting bugs etc...

Please use GitHub issues & pull requests. This project is licensed under the MIT license.
