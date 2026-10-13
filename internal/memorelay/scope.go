package memorelay

import (
	"regexp"
	"strings"
)

var (
	nonSlug = regexp.MustCompile(`[^a-z0-9.-]+`)
	dashRun = regexp.MustCompile(`-{2,}`)
)

// ScopeForRepo is the cairn scope a repository's memos are filed under, or "" when repo names
// none.
//
// cairn derives a scope from the basename of a clone's directory, normalised (lower-cased, every
// run outside `[a-z0-9.-]` folded to one `-`, leading and trailing `-` trimmed). A dispatched
// agent has no clone to ask, only the `owner/name` it was dispatched for, so the NAME stands in
// for the directory: a clone made with the default `git clone` is named after the repository.
//
// ⚠ A CLONE KEPT UNDER ANOTHER DIRECTORY NAME FILES ITS MEMOS UNDER THAT NAME, and this function
// cannot know it. Such a repository's agents ask for a scope that either does not exist or holds
// nothing — the listener answers its uniform miss, which the relay counts and does not retry for
// [UnreadableBackoff]. Nothing is guessed.
func ScopeForRepo(repo string) string {
	name := strings.TrimSpace(repo)
	if i := strings.LastIndex(name, "/"); i >= 0 {
		name = name[i+1:]
	}
	name = strings.TrimSuffix(name, ".git")
	s := nonSlug.ReplaceAllString(strings.ToLower(name), "-")
	s = dashRun.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-")
	// A name of dots alone ("." or "..") is a path, not a repository.
	if strings.Trim(s, ".") == "" {
		return ""
	}
	return s
}
