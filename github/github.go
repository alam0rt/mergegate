// Package github fetches pull requests and gate configuration from the
// GitHub REST API.
package github

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/alam0rt/mergegate"
)

// ConfigPath is where the gate looks for per-repository configuration.
const ConfigPath = ".mergegate.yaml"

// Client is a minimal GitHub REST client.
type Client struct {
	BaseURL string // e.g. https://api.github.com
	Token   string
	HTTP    *http.Client
}

// New returns a client for api.github.com.
func New(token string) *Client {
	return &Client{BaseURL: "https://api.github.com", Token: token, HTTP: http.DefaultClient}
}

var (
	shortRef = regexp.MustCompile(`^([\w.-]+/[\w.-]+)#(\d+)$`)
	urlRef   = regexp.MustCompile(`^https://github\.com/([\w.-]+/[\w.-]+)/pull/(\d+)(?:/.*)?$`)
)

// ParseRef accepts owner/repo#123 or a github.com pull request URL.
func ParseRef(s string) (repo string, number int, err error) {
	for _, re := range []*regexp.Regexp{shortRef, urlRef} {
		if m := re.FindStringSubmatch(s); m != nil {
			n, err := strconv.Atoi(m[2])
			if err != nil {
				return "", 0, err
			}
			return m[1], n, nil
		}
	}
	return "", 0, fmt.Errorf("%q is not owner/repo#N or a pull request URL", s)
}

type apiPR struct {
	Number int    `json:"number"`
	Title  string `json:"title"`
	Body   string `json:"body"`
	Draft  bool   `json:"draft"`
	User   struct {
		Login string `json:"login"`
		Type  string `json:"type"`
	} `json:"user"`
	Base struct {
		Ref string `json:"ref"`
		SHA string `json:"sha"`
	} `json:"base"`
	Head struct {
		SHA string `json:"sha"`
	} `json:"head"`
	Labels []struct {
		Name string `json:"name"`
	} `json:"labels"`
}

type apiUser struct {
	Login string `json:"login"`
}

type apiComment struct {
	User        apiUser   `json:"user"`
	Association string    `json:"author_association"`
	Body        string    `json:"body"`
	CreatedAt   time.Time `json:"created_at"`
}

type apiReview struct {
	User        apiUser   `json:"user"`
	Association string    `json:"author_association"`
	State       string    `json:"state"`
	Body        string    `json:"body"`
	SubmittedAt time.Time `json:"submitted_at"`
}

type apiCommit struct {
	SHA    string `json:"sha"`
	Commit struct {
		Message   string `json:"message"`
		Committer struct {
			Date time.Time `json:"date"`
		} `json:"committer"`
	} `json:"commit"`
}

type apiFile struct {
	Filename         string `json:"filename"`
	PreviousFilename string `json:"previous_filename"`
	Status           string `json:"status"`
	Additions        int    `json:"additions"`
	Deletions        int    `json:"deletions"`
	Patch            string `json:"patch"`
}

// errNotFound is returned by get for a 404.
var errNotFound = fmt.Errorf("not found")

// get fetches u and returns the body and the next-page URL, if any.
func (c *Client) get(ctx context.Context, u, accept string) ([]byte, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("Accept", accept)
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, "", err
	}
	if resp.StatusCode == http.StatusNotFound {
		return nil, "", errNotFound
	}
	if resp.StatusCode/100 != 2 {
		return nil, "", fmt.Errorf("GET %s: %s: %s", req.URL.Path, resp.Status, body)
	}
	return body, nextLink(resp.Header.Get("Link")), nil
}

var nextRe = regexp.MustCompile(`<([^>]+)>;\s*rel="next"`)

func nextLink(h string) string {
	if m := nextRe.FindStringSubmatch(h); m != nil {
		return m[1]
	}
	return ""
}

// PullRequest fetches a pull request and all of its changed files.
func (c *Client) PullRequest(ctx context.Context, repo string, number int) (mergegate.PullRequest, error) {
	base := fmt.Sprintf("%s/repos/%s/pulls/%d", c.BaseURL, repo, number)
	body, _, err := c.get(ctx, base, "application/vnd.github+json")
	if err != nil {
		return mergegate.PullRequest{}, fmt.Errorf("fetch %s#%d: %w", repo, number, err)
	}
	var p apiPR
	if err := json.Unmarshal(body, &p); err != nil {
		return mergegate.PullRequest{}, fmt.Errorf("decode %s#%d: %w", repo, number, err)
	}
	pr := mergegate.PullRequest{
		Repo: repo, Number: p.Number, Title: p.Title, Body: p.Body, Draft: p.Draft,
		Author: p.User.Login, IsBot: p.User.Type == "Bot",
		BaseRef: p.Base.Ref, BaseSHA: p.Base.SHA, HeadSHA: p.Head.SHA,
	}
	for _, l := range p.Labels {
		pr.Labels = append(pr.Labels, l.Name)
	}

	next := base + "/files?per_page=100&page=1"
	for next != "" {
		var page []apiFile
		body, next, err = c.get(ctx, next, "application/vnd.github+json")
		if err != nil {
			return mergegate.PullRequest{}, fmt.Errorf("fetch files for %s#%d: %w", repo, number, err)
		}
		if err := json.Unmarshal(body, &page); err != nil {
			return mergegate.PullRequest{}, fmt.Errorf("decode files for %s#%d: %w", repo, number, err)
		}
		for _, f := range page {
			pr.Files = append(pr.Files, mergegate.File{
				Path: f.Filename, PreviousPath: f.PreviousFilename, Status: f.Status,
				Additions: f.Additions, Deletions: f.Deletions, Patch: f.Patch,
			})
		}
	}

	pr.Comments, pr.Reviews, err = c.discussion(ctx, repo, number)
	if err != nil {
		return mergegate.PullRequest{}, fmt.Errorf("%s#%d: %w", repo, number, err)
	}
	return pr, nil
}

// ConfigFile returns ConfigPath from repo at ref, or nil if it does not exist.
func (c *Client) ConfigFile(ctx context.Context, repo, ref string) ([]byte, error) {
	u := fmt.Sprintf("%s/repos/%s/contents/%s?ref=%s", c.BaseURL, repo, ConfigPath, url.QueryEscape(ref))
	body, _, err := c.get(ctx, u, "application/vnd.github.raw+json")
	if err == errNotFound {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("fetch %s: %w", ConfigPath, err)
	}
	return body, nil
}

// getAll fetches every page of a JSON array endpoint.
func getAll[T any](ctx context.Context, c *Client, u string) ([]T, error) {
	var all []T
	for u != "" {
		body, next, err := c.get(ctx, u, "application/vnd.github+json")
		if err != nil {
			return nil, err
		}
		var page []T
		if err := json.Unmarshal(body, &page); err != nil {
			return nil, fmt.Errorf("decode %s: %w", u, err)
		}
		all = append(all, page...)
		u = next
	}
	return all, nil
}

// discussion fetches conversation comments, reviews and inline review
// comments. Review bodies are folded into the comments so the conversation
// reads in order.
func (c *Client) discussion(ctx context.Context, repo string, number int) ([]mergegate.Comment, []mergegate.Review, error) {
	issue, err := getAll[apiComment](ctx, c, fmt.Sprintf("%s/repos/%s/issues/%d/comments?per_page=100", c.BaseURL, repo, number))
	if err != nil {
		return nil, nil, fmt.Errorf("fetch comments: %w", err)
	}
	reviews, err := getAll[apiReview](ctx, c, fmt.Sprintf("%s/repos/%s/pulls/%d/reviews?per_page=100", c.BaseURL, repo, number))
	if err != nil {
		return nil, nil, fmt.Errorf("fetch reviews: %w", err)
	}
	inline, err := getAll[apiComment](ctx, c, fmt.Sprintf("%s/repos/%s/pulls/%d/comments?per_page=100", c.BaseURL, repo, number))
	if err != nil {
		return nil, nil, fmt.Errorf("fetch review comments: %w", err)
	}

	var comments []mergegate.Comment
	add := func(kind string, cm apiComment) {
		comments = append(comments, mergegate.Comment{
			Author: cm.User.Login, Association: cm.Association, Body: cm.Body, Kind: kind, CreatedAt: cm.CreatedAt,
		})
	}
	for _, cm := range issue {
		add("comment", cm)
	}
	for _, cm := range inline {
		add("review_comment", cm)
	}
	var rs []mergegate.Review
	for _, r := range reviews {
		rs = append(rs, mergegate.Review{Author: r.User.Login, Association: r.Association, State: r.State, SubmittedAt: r.SubmittedAt})
		if strings.TrimSpace(r.Body) != "" {
			add("review", apiComment{User: r.User, Association: r.Association, Body: r.Body, CreatedAt: r.SubmittedAt})
		}
	}
	slices.SortStableFunc(comments, func(a, b mergegate.Comment) int { return a.CreatedAt.Compare(b.CreatedAt) })
	slices.SortStableFunc(rs, func(a, b mergegate.Review) int { return a.SubmittedAt.Compare(b.SubmittedAt) })
	return comments, rs, nil
}

// maxHistoryPaths caps the per-file history requests for very wide PRs.
const maxHistoryPaths = 20

// History returns up to n recent commits on ref, newest first. With
// changedFilesOnly, only commits touching paths count.
func (c *Client) History(ctx context.Context, repo, ref string, paths []string, n int, changedFilesOnly bool) ([]mergegate.Commit, error) {
	base := fmt.Sprintf("%s/repos/%s/commits?sha=%s&per_page=%d", c.BaseURL, repo, url.QueryEscape(ref), n)
	var urls []string
	if !changedFilesOnly || len(paths) == 0 {
		urls = []string{base}
	} else {
		for _, p := range paths[:min(len(paths), maxHistoryPaths)] {
			urls = append(urls, base+"&path="+url.QueryEscape(p))
		}
	}

	seen := map[string]bool{}
	var out []mergegate.Commit
	for _, u := range urls {
		// One page is enough: each request already asks for n commits.
		body, _, err := c.get(ctx, u, "application/vnd.github+json")
		if err != nil {
			return nil, fmt.Errorf("fetch history: %w", err)
		}
		var page []apiCommit
		if err := json.Unmarshal(body, &page); err != nil {
			return nil, fmt.Errorf("decode history: %w", err)
		}
		for _, cm := range page {
			if !seen[cm.SHA] {
				seen[cm.SHA] = true
				out = append(out, mergegate.Commit{SHA: cm.SHA, Message: cm.Commit.Message, Date: cm.Commit.Committer.Date})
			}
		}
	}
	slices.SortStableFunc(out, func(a, b mergegate.Commit) int { return b.Date.Compare(a.Date) })
	return out[:min(len(out), n)], nil
}
