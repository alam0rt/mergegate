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
	"strconv"

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
	} `json:"base"`
	Head struct {
		SHA string `json:"sha"`
	} `json:"head"`
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
		BaseRef: p.Base.Ref, HeadSHA: p.Head.SHA,
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
