package github

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/alam0rt/mergegate"
)

func newServer(t *testing.T, routes map[string]http.HandlerFunc) *Client {
	t.Helper()
	// Discussion endpoints default to empty so tests only stub what they need.
	for _, n := range []string{"219", "9"} {
		for _, p := range []string{"GET /repos/alam0rt/flux/issues/%s/comments", "GET /repos/alam0rt/flux/pulls/%s/reviews", "GET /repos/alam0rt/flux/pulls/%s/comments"} {
			if _, ok := routes[fmt.Sprintf(p, n)]; !ok {
				routes[fmt.Sprintf(p, n)] = func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "[]") }
			}
		}
	}
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
		Author: "github-actions[bot]", IsBot: true, BaseRef: "main", BaseSHA: "base123", HeadSHA: "head456",
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

func TestPullRequestReadsDiscussion(t *testing.T) {
	c := newServer(t, map[string]http.HandlerFunc{
		"GET /repos/alam0rt/flux/pulls/219": func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, strings.Replace(prJSON, `"draft": false`, `"draft": false, "labels": [{"name": "hold"}, {"name": "deps"}]`, 1))
		},
		"GET /repos/alam0rt/flux/pulls/219/files": func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "[]") },
		"GET /repos/alam0rt/flux/issues/219/comments": func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, `[{"user":{"login":"sam"},"author_association":"OWNER","body":"/hold","created_at":"2026-09-27T10:00:00Z"}]`)
		},
		"GET /repos/alam0rt/flux/pulls/219/reviews": func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, `[{"user":{"login":"bob"},"author_association":"MEMBER","state":"CHANGES_REQUESTED","body":"needs a migration","submitted_at":"2026-09-27T09:00:00Z"},
			              {"user":{"login":"amy"},"author_association":"MEMBER","state":"APPROVED","body":"","submitted_at":"2026-09-27T11:00:00Z"}]`)
		},
		"GET /repos/alam0rt/flux/pulls/219/comments": func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, `[{"user":{"login":"bob"},"author_association":"MEMBER","body":"this line","created_at":"2026-09-27T09:30:00Z"}]`)
		},
	})
	pr, err := c.PullRequest(context.Background(), "alam0rt/flux", 219)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(pr.Labels, []string{"hold", "deps"}) {
		t.Errorf("Labels = %v", pr.Labels)
	}
	// Review bodies and inline comments join the conversation, oldest first.
	var got []string
	for _, cm := range pr.Comments {
		got = append(got, cm.Kind+":"+cm.Author+":"+cm.Body+":"+cm.Association)
	}
	want := []string{
		"review:bob:needs a migration:MEMBER",
		"review_comment:bob:this line:MEMBER",
		"comment:sam:/hold:OWNER",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Comments =\n%q\nwant\n%q", got, want)
	}
	if len(pr.Reviews) != 2 || pr.Reviews[0].State != "CHANGES_REQUESTED" || pr.Reviews[1].Author != "amy" {
		t.Errorf("Reviews = %+v", pr.Reviews)
	}
}

func TestHistoryForChangedFiles(t *testing.T) {
	c := newServer(t, map[string]http.HandlerFunc{
		"GET /repos/o/r/commits": func(w http.ResponseWriter, r *http.Request) {
			q := r.URL.Query()
			if q.Get("sha") != "main" || q.Get("per_page") != "2" {
				t.Errorf("query = %v", q)
			}
			switch q.Get("path") {
			case "a.yaml":
				fmt.Fprint(w, `[{"sha":"c3","commit":{"message":"bump a","committer":{"date":"2026-09-03T00:00:00Z"}}},
				               {"sha":"c1","commit":{"message":"Revert \"bump a\"","committer":{"date":"2026-09-01T00:00:00Z"}}}]`)
			case "b.yaml":
				fmt.Fprint(w, `[{"sha":"c3","commit":{"message":"bump a","committer":{"date":"2026-09-03T00:00:00Z"}}},
				               {"sha":"c2","commit":{"message":"tweak b","committer":{"date":"2026-09-02T00:00:00Z"}}}]`)
			default:
				t.Errorf("unexpected path %q", q.Get("path"))
			}
		},
	})
	got, err := c.History(context.Background(), "o/r", "main", []string{"a.yaml", "b.yaml"}, 2, true)
	if err != nil {
		t.Fatal(err)
	}
	// Deduplicated across files, newest first, capped at n.
	if len(got) != 2 || got[0].SHA != "c3" || got[1].SHA != "c2" {
		t.Errorf("History = %+v", got)
	}
}

func TestHistoryWholeBranch(t *testing.T) {
	c := newServer(t, map[string]http.HandlerFunc{
		"GET /repos/o/r/commits": func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Query().Has("path") {
				t.Error("branch-wide history should not filter by path")
			}
			fmt.Fprint(w, `[{"sha":"c9","commit":{"message":"x","committer":{"date":"2026-09-03T00:00:00Z"}}}]`)
		},
	})
	got, err := c.History(context.Background(), "o/r", "main", []string{"a.yaml"}, 5, false)
	if err != nil || len(got) != 1 || got[0].Message != "x" {
		t.Errorf("History = %+v, %v", got, err)
	}
}
