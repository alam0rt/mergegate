package rules

import (
	"reflect"
	"strings"
	"testing"

	"github.com/alam0rt/mergegate"
)

func files(paths ...string) []mergegate.File {
	var fs []mergegate.File
	for _, p := range paths {
		fs = append(fs, mergegate.File{Path: p, Status: "modified", Additions: 1, Deletions: 1, Patch: "@@ -1 +1 @@\n-a\n+b"})
	}
	return fs
}

func TestDocsOnly(t *testing.T) {
	cfg := Default()
	cases := []struct {
		name  string
		paths []string
		want  bool
	}{
		{"root readme", []string{"README.md"}, true},
		{"nested markdown", []string{"clusters/omar/NOTES.md"}, true},
		{"docs dir with image", []string{"docs/arch.png", "docs/index.md"}, true},
		{"license", []string{"LICENSE"}, true},
		{"yaml is not docs", []string{"clusters/omar/helm-velero.yaml"}, false},
		{"mixed", []string{"README.md", "main.go"}, false},
		{"requirements.txt is not docs", []string{"requirements.txt"}, false},
		{"no files", nil, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Check(cfg, mergegate.PullRequest{Files: files(c.paths...)}).DocsOnly
			if got != c.want {
				t.Errorf("DocsOnly = %v, want %v", got, c.want)
			}
		})
	}
}

func TestRenameOutOfDocsIsNotDocsOnly(t *testing.T) {
	pr := mergegate.PullRequest{Files: []mergegate.File{{
		Path: "src/notes.go", PreviousPath: "docs/notes.go", Status: "renamed",
	}}}
	if Check(Default(), pr).DocsOnly {
		t.Error("a file renamed from docs/ into src/ must not count as docs-only")
	}
}

func TestProtectedPaths(t *testing.T) {
	pr := mergegate.PullRequest{Files: files(
		"README.md",
		".github/workflows/ci.yaml",
		".mergegate.yaml",
	)}
	got := Check(Default(), pr).Protected
	want := []string{".github/workflows/ci.yaml", ".mergegate.yaml"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Protected = %v, want %v", got, want)
	}
}

func TestProtectedCatchesRenameSource(t *testing.T) {
	pr := mergegate.PullRequest{Files: []mergegate.File{{
		Path: "old-ci.yaml", PreviousPath: ".github/workflows/ci.yaml", Status: "renamed",
	}}}
	if got := Check(Default(), pr).Protected; len(got) != 1 {
		t.Errorf("Protected = %v, want the renamed workflow", got)
	}
}

func TestTooLarge(t *testing.T) {
	cfg := Default()
	cfg.MaxChangedLines = 10
	pr := mergegate.PullRequest{Files: []mergegate.File{{Path: "a.yaml", Additions: 6, Deletions: 5, Patch: "x"}}}
	if !Check(cfg, pr).TooLarge {
		t.Error("11 changed lines should exceed a limit of 10")
	}
	pr.Files[0].Deletions = 4
	if Check(cfg, pr).TooLarge {
		t.Error("10 changed lines should not exceed a limit of 10")
	}
}

func TestMissingPatchOutsideDocs(t *testing.T) {
	pr := mergegate.PullRequest{Files: []mergegate.File{
		{Path: "docs/diagram.png", Status: "added"},
		{Path: "bin/tool", Status: "added"},
	}}
	got := Check(Default(), pr).Unreadable
	if !reflect.DeepEqual(got, []string{"bin/tool"}) {
		t.Errorf("Unreadable = %v, want [bin/tool]", got)
	}
}

func TestAuthorAllowlist(t *testing.T) {
	cfg := Default()
	if !Check(cfg, mergegate.PullRequest{Author: "anyone"}).AuthorAllowed {
		t.Error("an empty allowlist should allow any author")
	}
	cfg.AllowedAuthors = []string{"github-actions[bot]", "renovate[bot]"}
	if Check(cfg, mergegate.PullRequest{Author: "mallory"}).AuthorAllowed {
		t.Error("mallory is not in the allowlist")
	}
	if !Check(cfg, mergegate.PullRequest{Author: "renovate[bot]"}).AuthorAllowed {
		t.Error("renovate[bot] is in the allowlist")
	}
}

func TestLoadMergesOverDefaults(t *testing.T) {
	cfg, err := Load(strings.NewReader(`
protected_paths:
  - "clusters/*/flux-system/**"
max_changed_lines: 50
allowed_authors: ["github-actions[bot]"]
`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MaxChangedLines != 50 {
		t.Errorf("MaxChangedLines = %d, want 50", cfg.MaxChangedLines)
	}
	if !reflect.DeepEqual(cfg.DocsPaths, Default().DocsPaths) {
		t.Error("unset docs_paths should keep the defaults")
	}
	// Protected paths from the file are added to the defaults, never
	// replace them, so a repo cannot un-protect its own gate config.
	pr := mergegate.PullRequest{Files: files("clusters/omar/flux-system/gotk-components.yaml", ".mergegate.yaml")}
	if got := Check(cfg, pr).Protected; len(got) != 2 {
		t.Errorf("Protected = %v, want both files", got)
	}
}

func TestLoadRejectsUnknownKeys(t *testing.T) {
	if _, err := Load(strings.NewReader("protect_paths: [x]\n")); err == nil {
		t.Error("a misspelt key should be an error, not silently ignored")
	}
}

func TestLoadRejectsBadGlob(t *testing.T) {
	if _, err := Load(strings.NewReader("docs_paths: [\"[\"]\n")); err == nil {
		t.Error("an invalid glob should be an error")
	}
}

func TestLoadWatches(t *testing.T) {
	cfg, err := Load(strings.NewReader(`
watch:
  - id: ingress_snippets
    question: "Does this diff add nginx snippet annotations?"
    paths: ["clusters/**"]
    threshold: 0.3
  - id: db_migration
    question: "Does this change require a database migration?"
`))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Watches) != 2 {
		t.Fatalf("got %d watches, want 2", len(cfg.Watches))
	}
	if w := cfg.Watches[0]; w.ID != "ingress_snippets" || w.Limit(cfg.Thresholds) != 0.3 || len(w.Paths) != 1 {
		t.Errorf("first watch = %+v", w)
	}
	if got := cfg.Watches[1].Limit(cfg.Thresholds); got != cfg.Thresholds.No {
		t.Errorf("default limit = %v, want thresholds.no %v", got, cfg.Thresholds.No)
	}
}

func TestLoadRejectsBadWatches(t *testing.T) {
	cases := map[string]string{
		"missing id":     "watch: [{question: q}]",
		"bad id":         "watch: [{id: Bad-ID, question: q}]",
		"duplicate id":   "watch: [{id: a, question: q}, {id: a, question: r}]",
		"empty question": "watch: [{id: a}]",
		"threshold > 1":  "watch: [{id: a, question: q, threshold: 1.5}]",
		"bad glob":       `watch: [{id: a, question: q, paths: ["["]}]`,
		"misspelt field": "watch: [{id: a, question: q, path: [x]}]",
	}
	for name, doc := range cases {
		if _, err := Load(strings.NewReader(doc)); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
}

func TestWatchApplies(t *testing.T) {
	always := Watch{ID: "a", Question: "q"}
	scoped := Watch{ID: "b", Question: "q", Paths: []string{"clusters/**"}}
	inClusters := mergegate.PullRequest{Files: files("clusters/omar/x.yaml")}
	elsewhere := mergegate.PullRequest{Files: files("apps/x.yaml")}
	renamedOut := mergegate.PullRequest{Files: []mergegate.File{{Path: "apps/x.yaml", PreviousPath: "clusters/x.yaml"}}}

	if !always.Applies(elsewhere) {
		t.Error("a watch without paths applies to every PR")
	}
	if !scoped.Applies(inClusters) || scoped.Applies(elsewhere) {
		t.Error("a scoped watch applies only when a file matches")
	}
	if !scoped.Applies(renamedOut) {
		t.Error("a rename source counts as a match")
	}
}
