package github

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/alam0rt/mergegate"
)

func newServer(t *testing.T, routes map[string]http.HandlerFunc) *Client {
	t.Helper()
	mux := http.NewServeMux()
	for pattern, h := range routes {
		mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
			if got := r.Header.Get("Authorization"); got != "Bearer tok" {
				t.Errorf("%s: Authorization = %q", r.URL.Path, got)
			}
			h(w, r)
		})
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &Client{BaseURL: srv.URL, Token: "tok", HTTP: srv.Client()}
}

const prJSON = `{
  "number": 219, "title": "chore(images): update element", "body": "bump",
  "draft": false, "user": {"login": "github-actions[bot]", "type": "Bot"},
  "base": {"ref": "main", "sha": "base123"}, "head": {"sha": "head456"}
}`

func TestPullRequestPaginatesFiles(t *testing.T) {
	var base string
	c := newServer(t, map[string]http.HandlerFunc{
		"GET /repos/alam0rt/flux/pulls/219": func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, prJSON)
		},
		"GET /repos/alam0rt/flux/pulls/219/files": func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Query().Get("page") {
			case "1":
				w.Header().Set("Link", fmt.Sprintf(`<%s/repos/alam0rt/flux/pulls/219/files?per_page=100&page=2>; rel="next"`, base))
				fmt.Fprint(w, `[{"filename":"a.yaml","status":"modified","additions":1,"deletions":1,"patch":"-x\n+y"}]`)
			case "2":
				fmt.Fprint(w, `[{"filename":"new.md","previous_filename":"old.md","status":"renamed","additions":0,"deletions":0}]`)
			default:
				t.Errorf("unexpected page %q", r.URL.Query().Get("page"))
			}
		},
	})
	base = c.BaseURL

	got, err := c.PullRequest(context.Background(), "alam0rt/flux", 219)
	if err != nil {
		t.Fatal(err)
	}
	want := mergegate.PullRequest{
		Repo: "alam0rt/flux", Number: 219, Title: "chore(images): update element", Body: "bump",
		Author: "github-actions[bot]", IsBot: true, BaseRef: "main", HeadSHA: "head456",
		Files: []mergegate.File{
			{Path: "a.yaml", Status: "modified", Additions: 1, Deletions: 1, Patch: "-x\n+y"},
			{Path: "new.md", PreviousPath: "old.md", Status: "renamed"},
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got\n%+v\nwant\n%+v", got, want)
	}
}

func TestPullRequestNotFound(t *testing.T) {
	c := newServer(t, map[string]http.HandlerFunc{
		"GET /repos/o/r/pulls/9": func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, `{"message":"Not Found"}`, 404)
		},
	})
	if _, err := c.PullRequest(context.Background(), "o/r", 9); err == nil {
		t.Error("want an error for a 404")
	}
}

func TestConfigFileReadsBaseRef(t *testing.T) {
	c := newServer(t, map[string]http.HandlerFunc{
		"GET /repos/o/r/contents/.mergegate.yaml": func(w http.ResponseWriter, r *http.Request) {
			if ref := r.URL.Query().Get("ref"); ref != "main" {
				t.Errorf("ref = %q, want main", ref)
			}
			if acc := r.Header.Get("Accept"); acc != "application/vnd.github.raw+json" {
				t.Errorf("Accept = %q", acc)
			}
			fmt.Fprint(w, "max_changed_lines: 5\n")
		},
	})
	b, err := c.ConfigFile(context.Background(), "o/r", "main")
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "max_changed_lines: 5\n" {
		t.Errorf("got %q", b)
	}
}

func TestConfigFileMissingIsNil(t *testing.T) {
	c := newServer(t, map[string]http.HandlerFunc{
		"GET /repos/o/r/contents/.mergegate.yaml": func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, `{"message":"Not Found"}`, 404)
		},
	})
	b, err := c.ConfigFile(context.Background(), "o/r", "main")
	if err != nil || b != nil {
		t.Errorf("got (%q, %v), want (nil, nil)", b, err)
	}
}

func TestParseRef(t *testing.T) {
	cases := []struct {
		in   string
		repo string
		n    int
		ok   bool
	}{
		{"alam0rt/flux#219", "alam0rt/flux", 219, true},
		{"https://github.com/alam0rt/flux/pull/219", "alam0rt/flux", 219, true},
		{"https://github.com/alam0rt/flux/pull/219/files", "alam0rt/flux", 219, true},
		{"alam0rt/flux", "", 0, false},
		{"flux#219", "", 0, false},
		{"alam0rt/flux#abc", "", 0, false},
	}
	for _, c := range cases {
		repo, n, err := ParseRef(c.in)
		if (err == nil) != c.ok || repo != c.repo || n != c.n {
			t.Errorf("ParseRef(%q) = (%q, %d, %v), want (%q, %d, ok=%v)", c.in, repo, n, err, c.repo, c.n, c.ok)
		}
	}
}
