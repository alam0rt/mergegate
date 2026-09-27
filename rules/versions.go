package rules

import (
	"cmp"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/alam0rt/mergegate"
)

// Versions configures the deterministic version check.
type Versions struct {
	// ZeroMinorIsMajor treats a 0.x minor bump (0.4 -> 0.5) as major, as
	// semver does. Turn it off for ecosystems that bump 0.x minors for
	// compatible releases (golang.org/x/*, for example).
	ZeroMinorIsMajor bool `yaml:"zero_minor_is_major"`
}

// VersionChange is one version that a diff line replaces with another.
type VersionChange struct {
	File     string
	Old, New string
	Kind     string // patch, minor, major or downgrade
}

var versionRe = regexp.MustCompile(`v?\d+(?:\.\d+)+`)

type version struct {
	text string
	nums [3]int
	at   int // byte offset in the line
}

// versions finds semver-like tokens (2 or 3 numeric parts) in a line,
// skipping IP addresses and parts of longer words.
func versions(line string) []version {
	var out []version
	for _, m := range versionRe.FindAllStringIndex(line, -1) {
		start, end := m[0], m[1]
		if start > 0 && (isAlnum(line[start-1]) || line[start-1] == '.') {
			continue
		}
		// "1.2.3.4" is an address and "1.2.3a" part of a word; "-rc1" is fine.
		if end < len(line) && (line[end] == '.' || isAlnum(line[end])) {
			continue
		}
		text := line[start:end]
		parts := strings.Split(strings.TrimPrefix(text, "v"), ".")
		if len(parts) > 3 {
			continue
		}
		v := version{text: text, at: start}
		for i, p := range parts {
			v.nums[i], _ = strconv.Atoi(p)
		}
		out = append(out, v)
	}
	return out
}

func isAlnum(b byte) bool {
	return b >= '0' && b <= '9' || b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z'
}

func classify(old, new [3]int, zeroMinorIsMajor bool) string {
	switch c := cmp.Compare(old[0], new[0]); {
	case c > 0:
		return "downgrade"
	case c < 0:
		return "major"
	}
	switch c := cmp.Compare(old[1], new[1]); {
	case c > 0:
		return "downgrade"
	case c < 0 && old[0] == 0 && zeroMinorIsMajor:
		return "major"
	case c < 0:
		return "minor"
	}
	if old[2] > new[2] {
		return "downgrade"
	}
	return "patch"
}

// VersionChanges pairs each removed line with the added line that replaces
// it (by position within a change block, and only when the text before the
// first version matches) and reports every version that differs.
func VersionChanges(pr mergegate.PullRequest, zeroMinorIsMajor bool) []VersionChange {
	var out []VersionChange
	for _, f := range pr.Files {
		var removed, added []string
		flush := func() {
			for i := range min(len(removed), len(added)) {
				ov, nv := versions(removed[i]), versions(added[i])
				if len(ov) == 0 || len(nv) == 0 {
					continue
				}
				if strings.TrimSpace(removed[i][:ov[0].at]) != strings.TrimSpace(added[i][:nv[0].at]) {
					continue // a different thing, not a new version of the same one
				}
				for j := range min(len(ov), len(nv)) {
					if ov[j].nums == nv[j].nums {
						continue
					}
					out = append(out, VersionChange{
						File: f.Path, Old: ov[j].text, New: nv[j].text,
						Kind: classify(ov[j].nums, nv[j].nums, zeroMinorIsMajor),
					})
				}
			}
			removed, added = nil, nil
		}
		for _, line := range strings.Split(f.Patch, "\n") {
			switch {
			case strings.HasPrefix(line, "-"):
				if len(added) > 0 {
					flush() // a new block starts
				}
				removed = append(removed, line[1:])
			case strings.HasPrefix(line, "+"):
				added = append(added, line[1:])
			default:
				flush()
			}
		}
		flush()
	}
	return out
}

func riskyVersions(cfg Config, pr mergegate.PullRequest) []string {
	var out []string
	for _, c := range VersionChanges(pr, cfg.Versions.ZeroMinorIsMajor) {
		if c.Kind == "major" || c.Kind == "downgrade" {
			if s := fmt.Sprintf("%s: %s -> %s (%s)", c.File, c.Old, c.New, c.Kind); !slices.Contains(out, s) {
				out = append(out, s)
			}
		}
	}
	return out
}
