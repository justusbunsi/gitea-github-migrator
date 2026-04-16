package helpers

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/justusbunsi/gitea-github-migrator/internal/constants"
)

func Pointer[T any](v T) *T {
	return &v
}

func Conditional[T any](cond bool, truthyValue, falsyValue T) T {
	if cond {
		return truthyValue
	}
	return falsyValue
}

// SmartRenovateBodyTruncate considers too-long-to-handle Pull Requests as created by Renovate.
// Those Pull Requests tend to exceed the limit by providing a very large "Release Notes" section.
// To mitigate migration errors, we truncate those body contents similar to how Renovate handles the limit.
// See https://github.com/renovatebot/renovate/blob/bd7214d04be0cc5ea7a4ddd8f6b214d5af19b707/lib/modules/platform/github/index.ts#L110 and https://github.com/renovatebot/renovate/blob/bd7214d04be0cc5ea7a4ddd8f6b214d5af19b707/lib/modules/platform/utils/pr-body.ts#L10.
// In case the hard-coded Renovate regex does not match, the original input is being hard-limit truncated at the end.
func SmartRenovateBodyTruncate(str string) string {
	r := regexp.MustCompile(`(?ms)(?P<preNotes>.*### Release Notes)(?P<releaseNotes>.*\n\n</details>\n\n---\n\n)(?P<postNotes>.*)`)
	str = strings.ReplaceAll(str, "This pull request was migrated from Gitea", "This pull request was migrated from Gitea **and the description was truncated due to platform limits**")
	matches := r.FindStringSubmatch(str)
	if matches == nil {
		return str[:constants.GithubBodyLimit] + "..."
	}
	divider := "\n\n</details>\n\n---\n\n"
	preNotes := matches[r.SubexpIndex("preNotes")]
	releaseNotes := matches[r.SubexpIndex("releaseNotes")]
	postNotes := matches[r.SubexpIndex("postNotes")]
	availableLength := constants.GithubBodyLimit - (len(preNotes) + len(postNotes) + len(divider))
	if availableLength <= 0 {
		return str[:constants.GithubBodyLimit] + "..."
	}
	return fmt.Sprintf("%s%s%s%s", preNotes, releaseNotes[:availableLength], divider, postNotes)
}

func ParseProjectSlug(slug string) (owner, repo string, err error) {
	delimPosition := strings.LastIndex(slug, "/")
	if delimPosition == -1 {
		return "", "", fmt.Errorf("missing owner/repo delimiter (slash): %s", slug)
	}

	return slug[:delimPosition], slug[delimPosition+1:], nil
}
