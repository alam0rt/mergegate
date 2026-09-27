// Package rules holds the deterministic, path-based checks that run before
// (and independently of) the model. They are the part of the gate that a
// crafted PR description or diff cannot talk its way past.
package rules

import (
	"fmt"
	"io"
	"slices"

	"github.com/alam0rt/mergegate"
	"github.com/bmatcuk/doublestar/v4"
	"gopkg.in/yaml.v3"
)

// Config is the per-repository gate configuration, normally read from
// .mergegate.yaml.
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
	AllowedAuthors []string `yaml:"allowed_authors"`
	// Thresholds tune how sure the model has to be.
	Thresholds Thresholds `yaml:"thresholds"`
}

// Thresholds are the probability cut-offs the gate applies to model answers.
type Thresholds struct {
	// Yes is the minimum probability for a "this is safe" noul to count.
	Yes float64 `yaml:"yes"`
	// No is the maximum probability for a "this is risky" noul to count as no.
	No float64 `yaml:"no"`
	// Confidence is the minimum confidence for the change-kind choice.
	Confidence float64 `yaml:"confidence"`
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
	}
}

// fileConfig mirrors Config with pointers so Load can tell "unset" from zero.
type fileConfig struct {
	DocsPaths       []string    `yaml:"docs_paths"`
	ProtectedPaths  []string    `yaml:"protected_paths"`
	MaxChangedLines *int        `yaml:"max_changed_lines"`
	AllowedAuthors  []string    `yaml:"allowed_authors"`
	Thresholds      *Thresholds `yaml:"thresholds"`
}

// Load reads a YAML config and layers it over Default.
func Load(r io.Reader) (Config, error) {
	cfg := Default()
	var fc fileConfig
	dec := yaml.NewDecoder(r)
	dec.KnownFields(true)
	if err := dec.Decode(&fc); err != nil && err != io.EOF {
		return Config{}, fmt.Errorf("parse config: %w", err)
	}
	if fc.DocsPaths != nil {
		cfg.DocsPaths = fc.DocsPaths
	}
	cfg.ProtectedPaths = append(cfg.ProtectedPaths, fc.ProtectedPaths...)
	if fc.MaxChangedLines != nil {
		cfg.MaxChangedLines = *fc.MaxChangedLines
	}
	cfg.AllowedAuthors = fc.AllowedAuthors
	if fc.Thresholds != nil {
		cfg.Thresholds = *fc.Thresholds
	}
	for _, g := range slices.Concat(cfg.DocsPaths, cfg.ProtectedPaths) {
		if !doublestar.ValidatePattern(g) {
			return Config{}, fmt.Errorf("invalid glob %q", g)
		}
	}
	return cfg, nil
}

// Facts are the results of the deterministic checks.
type Facts struct {
	DocsOnly      bool     // every file (and rename source) is under DocsPaths
	Protected     []string // files matching ProtectedPaths
	Unreadable    []string // non-docs files with no patch (binary or too big)
	TooLarge      bool     // ChangedLines > MaxChangedLines
	AuthorAllowed bool
}

// Check runs the deterministic checks against a pull request.
func Check(cfg Config, pr mergegate.PullRequest) Facts {
	f := Facts{
		DocsOnly:      len(pr.Files) > 0,
		TooLarge:      pr.ChangedLines() > cfg.MaxChangedLines,
		AuthorAllowed: len(cfg.AllowedAuthors) == 0 || slices.Contains(cfg.AllowedAuthors, pr.Author),
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

func matchAny(globs []string, path string) bool {
	for _, g := range globs {
		if ok, _ := doublestar.Match(g, path); ok {
			return true
		}
	}
	return false
}
