package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alam0rt/mergegate"
	"github.com/alam0rt/mergegate/gate"
	"github.com/alam0rt/mergegate/judge"
)

type fakeSource struct {
	prs     map[int]mergegate.PullRequest
	config  []byte
	cfgRefs []string
}

func (f *fakeSource) PullRequest(_ context.Context, repo string, n int) (mergegate.PullRequest, error) {
	pr, ok := f.prs[n]
	if !ok {
		return pr, errors.New("not found")
	}
	pr.Repo, pr.Number = repo, n
	return pr, nil
}

func (f *fakeSource) ConfigFile(_ context.Context, repo, ref string) ([]byte, error) {
	f.cfgRefs = append(f.cfgRefs, ref)
	return f.config, nil
}

type fakeAssessor struct{ a judge.Assessment }

func (f fakeAssessor) Assess(context.Context, mergegate.PullRequest) (judge.Assessment, error) {
	return f.a, nil
}

var bump = judge.Assessment{
	VersionBump: 0.97, MajorBump: 0.02, OtherChanges: 0.03, Removal: 0.01,
	Kind: "dependency_patch", KindConfidence: 0.9,
}

func yamlPR() mergegate.PullRequest {
	return mergegate.PullRequest{BaseRef: "main", HeadSHA: "abc", Author: "bot", Files: []mergegate.File{
		{Path: "clusters/omar/app.yaml", Status: "modified", Additions: 1, Deletions: 1, Patch: "-v1.0.0\n+v1.0.1"},
	}}
}

func docsPR() mergegate.PullRequest {
	return mergegate.PullRequest{BaseRef: "main", Files: []mergegate.File{
		{Path: "README.md", Status: "modified", Additions: 1, Patch: "+hi"},
	}}
}

func runWith(t *testing.T, src *fakeSource, args ...string) (int, string, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code := run(context.Background(), args, &out, &errOut, src, func(string) gate.Assessor { return fakeAssessor{bump} })
	return code, out.String(), errOut.String()
}

func TestExitCodes(t *testing.T) {
	src := &fakeSource{prs: map[int]mergegate.PullRequest{1: docsPR(), 2: yamlPR()}}
	major := yamlPR()
	major.Draft = true
	src.prs[3] = major

	if code, out, _ := runWith(t, src, "o/r#1", "o/r#2"); code != 0 {
		t.Errorf("all safe: exit %d, want 0\n%s", code, out)
	}
	if code, out, _ := runWith(t, src, "o/r#1", "o/r#3"); code != 2 {
		t.Errorf("one needs review: exit %d, want 2\n%s", code, out)
	}
	if code, _, stderr := runWith(t, src, "o/r#99"); code != 1 || !strings.Contains(stderr, "o/r#99") {
		t.Errorf("fetch error: exit %d, want 1; stderr %q", code, stderr)
	}
	if code, _, _ := runWith(t, src); code != 1 {
		t.Errorf("no args: exit %d, want 1", code)
	}
}

func TestTextOutput(t *testing.T) {
	src := &fakeSource{prs: map[int]mergegate.PullRequest{2: yamlPR()}}
	_, out, _ := runWith(t, src, "o/r#2")
	for _, want := range []string{"o/r#2", "auto-merge", "dependency_patch"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

func TestJSONOutput(t *testing.T) {
	src := &fakeSource{prs: map[int]mergegate.PullRequest{1: docsPR(), 2: yamlPR()}}
	_, out, _ := runWith(t, src, "-json", "o/r#1", "o/r#2")
	var got []result
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, out)
	}
	if len(got) != 2 || !got[0].AutoMerge || got[0].Assessment != nil || got[1].Assessment == nil || got[1].HeadSHA != "abc" {
		t.Errorf("unexpected results: %+v", got)
	}
}

func TestConfigComesFromBaseRef(t *testing.T) {
	src := &fakeSource{
		prs:    map[int]mergegate.PullRequest{2: yamlPR()},
		config: []byte("protected_paths: [\"clusters/**\"]\n"),
	}
	code, out, _ := runWith(t, src, "o/r#2")
	if code != 2 || !strings.Contains(out, "protected") {
		t.Errorf("base-ref config should protect clusters/**: exit %d\n%s", code, out)
	}
	if len(src.cfgRefs) != 1 || src.cfgRefs[0] != "main" {
		t.Errorf("config fetched at %v, want [main]", src.cfgRefs)
	}
}

func TestLocalConfigFlagWins(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gate.yaml")
	if err := os.WriteFile(path, []byte("max_changed_lines: 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	src := &fakeSource{
		prs:    map[int]mergegate.PullRequest{2: yamlPR()},
		config: []byte("protected_paths: [\"clusters/**\"]\n"),
	}
	code, out, _ := runWith(t, src, "-config", path, "o/r#2")
	if code != 2 || !strings.Contains(out, "max_changed_lines 1") {
		t.Errorf("local config should apply: exit %d\n%s", code, out)
	}
	if len(src.cfgRefs) != 0 {
		t.Error("repo config should not be fetched when -config is given")
	}
}

func TestBadRepoConfigIsAnError(t *testing.T) {
	src := &fakeSource{prs: map[int]mergegate.PullRequest{2: yamlPR()}, config: []byte("nope: 1\n")}
	if code, _, stderr := runWith(t, src, "o/r#2"); code != 1 || !strings.Contains(stderr, "config") {
		t.Errorf("exit %d, stderr %q; a broken config must fail closed", code, stderr)
	}
}
