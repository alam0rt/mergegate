package gate

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/alam0rt/mergegate"
	"github.com/alam0rt/mergegate/judge"
	"github.com/alam0rt/mergegate/rules"
)

type fakeAssessor struct {
	a     judge.Assessment
	err   error
	calls int
}

func (f *fakeAssessor) Assess(context.Context, mergegate.PullRequest) (judge.Assessment, error) {
	f.calls++
	return f.a, f.err
}

// safeBump is what Jev says about a clean patch-level image bump.
var safeBump = judge.Assessment{
	DocsOnly: 0.02, VersionBump: 0.97, MajorBump: 0.03, OtherChanges: 0.04, Removal: 0.01,
	Kind: "dependency_patch", KindConfidence: 0.9,
}

func pr(paths ...string) mergegate.PullRequest {
	p := mergegate.PullRequest{Repo: "o/r", Number: 1, Author: "github-actions[bot]"}
	for _, path := range paths {
		p.Files = append(p.Files, mergegate.File{Path: path, Status: "modified", Additions: 1, Deletions: 1, Patch: "-a\n+b"})
	}
	return p
}

func run(t *testing.T, cfg rules.Config, p mergegate.PullRequest, a judge.Assessment) (Verdict, int) {
	t.Helper()
	f := &fakeAssessor{a: a}
	v, err := Evaluate(context.Background(), cfg, p, f)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if len(v.Reasons) == 0 {
		t.Error("every verdict should explain itself")
	}
	return v, f.calls
}

func wantReason(t *testing.T, v Verdict, substr string) {
	t.Helper()
	for _, r := range v.Reasons {
		if strings.Contains(r, substr) {
			return
		}
	}
	t.Errorf("no reason mentions %q; reasons: %q", substr, v.Reasons)
}

func TestDeterministicVerdictsSkipTheModel(t *testing.T) {
	cfg := rules.Default()
	cfg.MaxChangedLines = 10

	draft := pr("a.yaml")
	draft.Draft = true
	stranger := pr("a.yaml")
	stranger.Author = "mallory"
	allowCfg := cfg
	allowCfg.AllowedAuthors = []string{"github-actions[bot]"}
	binary := pr("a.yaml")
	binary.Files[0].Patch = ""
	big := pr("a.yaml")
	big.Files[0].Additions = 20

	cases := []struct {
		name      string
		cfg       rules.Config
		pr        mergegate.PullRequest
		autoMerge bool
		reason    string
	}{
		{"docs only by path", cfg, pr("README.md", "docs/setup.md"), true, "documentation"},
		{"draft", cfg, draft, false, "draft"},
		{"author not allowed", allowCfg, stranger, false, "mallory"},
		{"protected beats docs", cfg, pr("docs/a.md", ".github/workflows/ci.yaml"), false, ".github/workflows/ci.yaml"},
		{"no patch", cfg, binary, false, "a.yaml"},
		{"too large", cfg, big, false, "21 changed lines"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			v, calls := run(t, c.cfg, c.pr, safeBump)
			if v.AutoMerge != c.autoMerge {
				t.Errorf("AutoMerge = %v, want %v (reasons %q)", v.AutoMerge, c.autoMerge, v.Reasons)
			}
			if calls != 0 {
				t.Errorf("model called %d times, want 0", calls)
			}
			wantReason(t, v, c.reason)
		})
	}
}

func TestModelVerdicts(t *testing.T) {
	cfg := rules.Default()
	with := func(mut func(*judge.Assessment)) judge.Assessment {
		a := safeBump
		mut(&a)
		return a
	}
	cases := []struct {
		name      string
		a         judge.Assessment
		autoMerge bool
		reason    string
	}{
		{"patch bump", safeBump, true, "dependency_patch"},
		{"minor bump", with(func(a *judge.Assessment) { a.Kind = "dependency_minor" }), true, "dependency_minor"},
		{"major bump", with(func(a *judge.Assessment) { a.MajorBump = 0.8 }), false, "major"},
		{"major kind", with(func(a *judge.Assessment) { a.Kind = "dependency_major" }), false, "dependency_major"},
		{"bump plus config", with(func(a *judge.Assessment) { a.OtherChanges = 0.6 }), false, "other changes"},
		{"removal", with(func(a *judge.Assessment) { a.Removal = 0.5 }), false, "removes"},
		{"unsure it is only a bump", with(func(a *judge.Assessment) { a.VersionBump = 0.7 }), false, "version bump"},
		{"low kind confidence", with(func(a *judge.Assessment) { a.KindConfidence = 0.4 }), false, "confidence"},
		{"nouls and choice disagree", with(func(a *judge.Assessment) { a.Kind = "feature" }), false, "feature"},
		{"yaml comment only", judge.Assessment{
			DocsOnly: 0.95, VersionBump: 0.05, MajorBump: 0.01, OtherChanges: 0.03, Removal: 0.02,
			Kind: "docs", KindConfidence: 0.85,
		}, true, "documentation"},
		{"docs noul without docs kind", judge.Assessment{
			DocsOnly: 0.95, OtherChanges: 0.03, Kind: "configuration", KindConfidence: 0.85,
		}, false, "configuration"},
		{"exactly at thresholds", with(func(a *judge.Assessment) {
			a.VersionBump, a.MajorBump, a.OtherChanges, a.Removal, a.KindConfidence = 0.9, 0.1, 0.1, 0.1, 0.6
		}), true, "dependency_patch"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			v, calls := run(t, cfg, pr("clusters/omar/app.yaml"), c.a)
			if v.AutoMerge != c.autoMerge {
				t.Errorf("AutoMerge = %v, want %v (reasons %q)", v.AutoMerge, c.autoMerge, v.Reasons)
			}
			if calls != 1 {
				t.Errorf("model called %d times, want 1", calls)
			}
			if v.Assessment == nil {
				t.Error("verdict should carry the assessment")
			}
			wantReason(t, v, c.reason)
		})
	}
}

func TestModelErrorIsNotAMerge(t *testing.T) {
	f := &fakeAssessor{err: errors.New("boom")}
	v, err := Evaluate(context.Background(), rules.Default(), pr("a.yaml"), f)
	if err == nil {
		t.Fatal("want the model error returned")
	}
	if v.AutoMerge {
		t.Error("an error must never produce AutoMerge")
	}
}
