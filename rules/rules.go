// Package rules holds the configuration and the deterministic checks that
// run before (and independently of) the model. They are the part of the gate
// that a crafted PR description or diff cannot talk its way past.
package rules

import (
	"fmt"
	"io"
	"regexp"
	"slices"
	"strings"

	"github.com/alam0rt/mergegate"
	"github.com/bmatcuk/doublestar/v4"
	"gopkg.in/yaml.v3"
)

// Config is the per-repository gate configuration, normally read from
// .mergegate.yaml. Every field has a useful default; see Default.
type Config struct {
	// DocsPaths are globs for files that cannot affect how anything builds,
	// deploys or runs. A PR touching only these merges without a model call.
	DocsPaths []string `yaml:"docs_paths"`
	// ProtectedPaths always need a human. Globs from a config file are
	// added to the defaults rather than replacing them.
	ProtectedPaths []string `yaml:"protected_paths"`
	// MaxChangedLines is the largest diff (additions + deletions) the gate
	// will judge. Anything bigger goes to a human.
	MaxChangedLines int `yaml:"max_changed_lines"`
	// AllowedAuthors, when non-empty, limits auto-merge to these logins.
	AllowedAuthors []string `yaml:"allowed_authors,omitempty"`
	// Thresholds tune how sure the model has to be.
	Thresholds Thresholds `yaml:"thresholds"`
	// Versions configures the deterministic major/downgrade check.
	Versions Versions `yaml:"versions"`
	// Hold lists the explicit "do not merge" signals.
	Hold Hold `yaml:"hold"`
	// Context configures what besides the diff watches may read.
	Context Context `yaml:"context"`
	// Watches are yes/no questions for the model where yes sends the PR to
	// review. They can never approve one. File entries are merged into the
	// defaults by ID.
	Watches []Watch `yaml:"watch"`
}

// Thresholds are the probability cut-offs the gate applies to model answers.
type Thresholds struct {
	// Yes is the minimum probability for a "this is safe" noul to count.
	Yes float64 `yaml:"yes"`
	// No is the maximum probability for a "this is risky" noul to count as
	// no. It is also the default limit for watches.
	No float64 `yaml:"no"`
	// Confidence is the minimum confidence for the change-kind choice.
	Confidence float64 `yaml:"confidence"`
}

// Hold is the set of explicit signals that stop an auto-merge outright.
type Hold struct {
	// Labels hold the PR when any is applied (compared case-insensitively).
	Labels []string `yaml:"labels"`
	// Command, on a line of its own in a trusted comment, holds the PR until
	// a later "/un<command>" (e.g. /hold and /unhold). Empty disables it.
	Command string `yaml:"command"`
	// ChangesRequested holds the PR while any trusted reviewer's latest
	// review requests changes.
	ChangesRequested bool `yaml:"changes_requested"`
}

// Context configures the sources, besides the diff, that watches can read.
type Context struct {
	Comments Comments `yaml:"comments"`
	History  History  `yaml:"history"`
}

// Comments configures which PR discussion is trusted.
type Comments struct {
	// From lists the author_association values whose comments and reviews
	// count, both for Hold and as context. Everyone else is ignored.
	From []string `yaml:"from"`
	// Max is how many of the newest trusted comments watches may read.
	// 0 turns comments off as context (Hold still reads them).
	Max int `yaml:"max"`
}

// History configures the base-branch commit history watches may read.
type History struct {
	// Commits is how many recent commits to read. 0 turns history off.
	Commits int `yaml:"commits"`
	// ChangedFilesOnly limits history to commits touching the PR's files.
	ChangedFilesOnly bool `yaml:"changed_files_only"`
}

// Context sources a watch can name in Uses.
const (
	SourceComments = "comments"
	SourceHistory  = "history"
)

// Enabled reports whether a context source is switched on.
func (c Context) Enabled(source string) bool {
	switch source {
	case SourceComments:
		return c.Comments.Max > 0
	case SourceHistory:
		return c.History.Commits > 0
	}
	return false
}

// Watch is a yes/no question phrased so that "yes" means "a human should
// look". If the model's p(yes) exceeds the limit, the PR needs review.
type Watch struct {
	ID       string `yaml:"id"`
	Question string `yaml:"question,omitempty"`
	// Paths, when set, limit the watch to PRs where a changed file (or a
	// rename source) matches one of these globs.
	Paths []string `yaml:"paths,omitempty"`
	// Uses names context sources the question needs. Watches with Uses are
	// asked in a separate request per distinct set of sources, so text from
	// one source can never influence another watch or the built-in checks.
	Uses []string `yaml:"uses,omitempty"`
	// Threshold overrides Thresholds.No for this watch.
	Threshold *float64 `yaml:"threshold,omitempty"`
	// Disabled, in a config file, removes the default watch with this ID.
	Disabled bool `yaml:"disabled,omitempty"`
}

// Limit is the highest p(yes) at which the watch still passes.
func (w Watch) Limit(t Thresholds) float64 {
	if w.Threshold != nil {
		return *w.Threshold
	}
	return t.No
}

// Applies reports whether the watch should be asked about pr.
func (w Watch) Applies(pr mergegate.PullRequest) bool {
	if len(w.Paths) == 0 {
		return true
	}
	for _, f := range pr.Files {
		if matchAny(w.Paths, f.Path) || (f.PreviousPath != "" && matchAny(w.Paths, f.PreviousPath)) {
			return true
		}
	}
	return false
}

// Default returns the built-in configuration.
func Default() Config {
	return Config{
		DocsPaths: []string{
			"**/*.md", "**/*.mdx", "**/*.markdown", "**/*.rst", "**/*.adoc",
			"docs/**", "doc/**",
			"**/LICENSE", "**/LICENSE.*", "**/COPYING",
			".github/ISSUE_TEMPLATE/**", ".github/PULL_REQUEST_TEMPLATE*",
		},
		ProtectedPaths: []string{
			".mergegate.yaml",
			".github/workflows/**",
			".github/actions/**",
			"**/CODEOWNERS",
		},
		MaxChangedLines: 400,
		Thresholds:      Thresholds{Yes: 0.9, No: 0.1, Confidence: 0.6},
		Versions:        Versions{ZeroMinorIsMajor: true},
		Hold: Hold{
			Labels:           []string{"hold", "do-not-merge", "do not merge", "wip", "work in progress", "blocked"},
			Command:          "/hold",
			ChangesRequested: true,
		},
		Context: Context{
			Comments: Comments{From: []string{"OWNER", "MEMBER", "COLLABORATOR"}, Max: 20},
			History:  History{Commits: 10, ChangedFilesOnly: true},
		},
		Watches: []Watch{
			{
				// Context watches ask a fuzzier question than the diff checks, so they
				// get a looser limit than thresholds.no.
				ID: "unresolved_concern",
				Question: "Has a maintainer or reviewer raised a concern about this change, asked for changes, " +
					"or asked to wait, without it being resolved later in the discussion?",
				Uses:      []string{SourceComments},
				Threshold: ptr(0.3),
			},
			{
				ID: "recent_revert",
				Question: "Does the recent history show that something this pull request changes was recently " +
					"reverted, rolled back, or deliberately pinned to an older version?",
				Uses:      []string{SourceHistory},
				Threshold: ptr(0.3),
			},
		},
	}
}

var associations = []string{
	"OWNER", "MEMBER", "COLLABORATOR", "CONTRIBUTOR",
	"FIRST_TIME_CONTRIBUTOR", "FIRST_TIMER", "MANNEQUIN", "NONE",
}

// Load reads a YAML config and layers it over Default. Fields left out keep
// their defaults, including fields inside nested blocks.
func Load(r io.Reader) (Config, error) {
	def := Default()
	cfg := Default()
	// These two merge with the defaults instead of replacing them, so start
	// them empty and combine after decoding.
	cfg.ProtectedPaths, cfg.Watches = nil, nil

	dec := yaml.NewDecoder(r)
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil && err != io.EOF {
		return Config{}, fmt.Errorf("parse config: %w", err)
	}

	for _, p := range cfg.ProtectedPaths {
		if !slices.Contains(def.ProtectedPaths, p) {
			def.ProtectedPaths = append(def.ProtectedPaths, p)
		}
	}
	cfg.ProtectedPaths = def.ProtectedPaths

	watches, err := mergeWatches(def.Watches, cfg.Watches, cfg.Context)
	if err != nil {
		return Config{}, err
	}
	cfg.Watches = watches

	if err := validate(cfg); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

var watchID = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// mergeWatches applies file watches to the defaults: a matching ID replaces
// (or, with disabled, removes) the default; a new ID is appended.
func mergeWatches(defaults, file []Watch, ctx Context) ([]Watch, error) {
	out := slices.Clone(defaults)
	seen := map[string]bool{}
	for i, w := range file {
		if !watchID.MatchString(w.ID) {
			return nil, fmt.Errorf("watch %d: id %q must match %s", i, w.ID, watchID)
		}
		if seen[w.ID] {
			return nil, fmt.Errorf("watch %q: duplicate id", w.ID)
		}
		seen[w.ID] = true
		at := slices.IndexFunc(out, func(d Watch) bool { return d.ID == w.ID })

		if w.Disabled {
			if at < 0 {
				return nil, fmt.Errorf("watch %q: disabled, but there is no default watch with that id", w.ID)
			}
			out = slices.Delete(out, at, at+1)
			continue
		}
		if strings.TrimSpace(w.Question) == "" {
			return nil, fmt.Errorf("watch %q: question is empty", w.ID)
		}
		for _, s := range w.Uses {
			if s != SourceComments && s != SourceHistory {
				return nil, fmt.Errorf("watch %q: unknown source %q (want %s or %s)", w.ID, s, SourceComments, SourceHistory)
			}
			if !ctx.Enabled(s) {
				return nil, fmt.Errorf("watch %q: uses %s, which is turned off in context", w.ID, s)
			}
		}
		if at >= 0 {
			out[at] = w
		} else {
			out = append(out, w)
		}
	}
	return out, nil
}

func validate(cfg Config) error {
	for _, w := range cfg.Watches {
		if w.Threshold != nil && (*w.Threshold < 0 || *w.Threshold > 1) {
			return fmt.Errorf("watch %q: threshold %v is not between 0 and 1", w.ID, *w.Threshold)
		}
		for _, g := range w.Paths {
			if !doublestar.ValidatePattern(g) {
				return fmt.Errorf("watch %q: invalid glob %q", w.ID, g)
			}
		}
	}
	for _, g := range slices.Concat(cfg.DocsPaths, cfg.ProtectedPaths) {
		if !doublestar.ValidatePattern(g) {
			return fmt.Errorf("invalid glob %q", g)
		}
	}
	if cfg.Context.Comments.Max < 0 || cfg.Context.History.Commits < 0 {
		return fmt.Errorf("context: max and commits must not be negative")
	}
	for _, a := range cfg.Context.Comments.From {
		if !slices.Contains(associations, a) {
			return fmt.Errorf("context.comments.from: %q is not one of %s", a, strings.Join(associations, ", "))
		}
	}
	return nil
}

// Facts are the results of the deterministic checks.
type Facts struct {
	DocsOnly      bool     // every file (and rename source) is under DocsPaths
	Protected     []string // files matching ProtectedPaths
	Unreadable    []string // non-docs files with no patch (binary or too big)
	TooLarge      bool     // ChangedLines > MaxChangedLines
	AuthorAllowed bool
	Held          []string // why the PR is on hold, if it is
	RiskyVersions []string // major bumps and downgrades found in the diff
}

// Check runs the deterministic checks against a pull request.
func Check(cfg Config, pr mergegate.PullRequest) Facts {
	f := Facts{
		DocsOnly:      len(pr.Files) > 0,
		TooLarge:      pr.ChangedLines() > cfg.MaxChangedLines,
		AuthorAllowed: len(cfg.AllowedAuthors) == 0 || slices.Contains(cfg.AllowedAuthors, pr.Author),
		Held:          held(cfg, pr),
		RiskyVersions: riskyVersions(cfg, pr),
	}
	for _, file := range pr.Files {
		paths := []string{file.Path}
		if file.PreviousPath != "" {
			paths = append(paths, file.PreviousPath)
		}
		docs := true
		for _, p := range paths {
			if !matchAny(cfg.DocsPaths, p) {
				docs = false
			}
		}
		if !docs {
			f.DocsOnly = false
		}
		if slices.ContainsFunc(paths, func(p string) bool { return matchAny(cfg.ProtectedPaths, p) }) {
			f.Protected = append(f.Protected, file.Path)
		}
		if file.Patch == "" && file.Status != "removed" && !docs {
			f.Unreadable = append(f.Unreadable, file.Path)
		}
	}
	return f
}

func held(cfg Config, pr mergegate.PullRequest) []string {
	var reasons []string
	for _, l := range pr.Labels {
		if slices.ContainsFunc(cfg.Hold.Labels, func(h string) bool { return strings.EqualFold(h, l) }) {
			reasons = append(reasons, "label "+l)
		}
	}

	trusted := func(assoc string) bool { return slices.Contains(cfg.Context.Comments.From, assoc) }

	if cmd := cfg.Hold.Command; cmd != "" {
		undo := "/un" + strings.TrimPrefix(cmd, "/")
		by := ""
		for _, c := range pr.Comments {
			if !trusted(c.Association) {
				continue
			}
			for _, line := range strings.Split(c.Body, "\n") {
				switch strings.TrimSpace(line) {
				case cmd:
					by = c.Author
				case undo:
					by = ""
				}
			}
		}
		if by != "" {
			reasons = append(reasons, cmd+" by "+by)
		}
	}

	if cfg.Hold.ChangesRequested {
		latest := map[string]string{}
		var order []string
		for _, r := range pr.Reviews {
			if !trusted(r.Association) || r.State == "COMMENTED" || r.State == "PENDING" {
				continue
			}
			if _, ok := latest[r.Author]; !ok {
				order = append(order, r.Author)
			}
			latest[r.Author] = r.State
		}
		for _, who := range order {
			if latest[who] == "CHANGES_REQUESTED" {
				reasons = append(reasons, "changes requested by "+who)
			}
		}
	}
	return reasons
}

// TrustedComments returns the newest Context.Comments.Max comments from
// trusted authors, oldest first.
func TrustedComments(cfg Config, pr mergegate.PullRequest) []mergegate.Comment {
	var out []mergegate.Comment
	for _, c := range pr.Comments {
		if slices.Contains(cfg.Context.Comments.From, c.Association) {
			out = append(out, c)
		}
	}
	if n := cfg.Context.Comments.Max; len(out) > n {
		out = out[len(out)-n:]
	}
	return out
}

func matchAny(globs []string, path string) bool {
	for _, g := range globs {
		if ok, _ := doublestar.Match(g, path); ok {
			return true
		}
	}
	return false
}

func ptr[T any](v T) *T { return &v }
