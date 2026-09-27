// Command mergegate reports whether pull requests are safe to merge without
// review.
//
//	mergegate [-json] [-config FILE] [-model MODEL] owner/repo#N ...
//
// Exit status is 0 when every PR may auto-merge, 2 when any needs review,
// and 1 on error.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"github.com/alam0rt/mergegate"
	"github.com/alam0rt/mergegate/gate"
	"github.com/alam0rt/mergegate/github"
	"github.com/alam0rt/mergegate/judge"
	"github.com/alam0rt/mergegate/rules"
)

const (
	exitMerge  = 0
	exitError  = 1
	exitReview = 2
)

// source is where pull requests and their repository config come from.
type source interface {
	PullRequest(ctx context.Context, repo string, n int) (mergegate.PullRequest, error)
	ConfigFile(ctx context.Context, repo, ref string) ([]byte, error)
}

type result struct {
	Repo       string            `json:"repo"`
	Number     int               `json:"number"`
	HeadSHA    string            `json:"head_sha"`
	AutoMerge  bool              `json:"auto_merge"`
	Reasons    []string          `json:"reasons"`
	Assessment *judge.Assessment `json:"assessment,omitempty"`
}

func main() {
	gh := github.New(githubToken())
	newJudge := func(model string) gate.Assessor {
		return judge.New(os.Getenv("OPENROUTER_API_KEY"), judge.WithModel(model))
	}
	os.Exit(run(context.Background(), os.Args[1:], os.Stdout, os.Stderr, gh, newJudge))
}

func githubToken() string {
	for _, k := range []string{"GITHUB_TOKEN", "GH_TOKEN"} {
		if v := os.Getenv(k); v != "" {
			return v
		}
	}
	out, err := exec.Command("gh", "auth", "token").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer, src source, newAssessor func(model string) gate.Assessor) int {
	fs := flag.NewFlagSet("mergegate", flag.ContinueOnError)
	fs.SetOutput(stderr)
	asJSON := fs.Bool("json", false, "print results as JSON")
	cfgPath := fs.String("config", "", "local config file (default: "+github.ConfigPath+" from each PR's base branch)")
	model := fs.String("model", judge.DefaultModel, "Jev model to use")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "usage: mergegate [flags] owner/repo#N|PR-URL ...")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return exitError
	}
	if fs.NArg() == 0 {
		fs.Usage()
		return exitError
	}

	var local *rules.Config
	if *cfgPath != "" {
		f, err := os.Open(*cfgPath)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return exitError
		}
		cfg, err := rules.Load(f)
		f.Close()
		if err != nil {
			fmt.Fprintf(stderr, "%s: %v\n", *cfgPath, err)
			return exitError
		}
		local = &cfg
	}

	assessor := newAssessor(*model)
	code := exitMerge
	var results []result
	for _, ref := range fs.Args() {
		r, err := evaluate(ctx, ref, local, src, assessor)
		if err != nil {
			fmt.Fprintf(stderr, "%s: %v\n", ref, err)
			code = exitError
			continue
		}
		if !r.AutoMerge && code == exitMerge {
			code = exitReview
		}
		results = append(results, r)
		if !*asJSON {
			printText(stdout, r)
		}
	}
	if *asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		enc.Encode(results)
	}
	return code
}

func evaluate(ctx context.Context, ref string, local *rules.Config, src source, assessor gate.Assessor) (result, error) {
	repo, n, err := github.ParseRef(ref)
	if err != nil {
		return result{}, err
	}
	pr, err := src.PullRequest(ctx, repo, n)
	if err != nil {
		return result{}, err
	}
	cfg := rules.Default()
	if local != nil {
		cfg = *local
	} else {
		// Read the config from the base branch, never the PR head, so a PR
		// cannot relax the rules it is judged by.
		raw, err := src.ConfigFile(ctx, repo, pr.BaseRef)
		if err != nil {
			return result{}, err
		}
		if raw != nil {
			if cfg, err = rules.Load(bytes.NewReader(raw)); err != nil {
				return result{}, fmt.Errorf("%s@%s: %w", github.ConfigPath, pr.BaseRef, err)
			}
		}
	}
	v, err := gate.Evaluate(ctx, cfg, pr, assessor)
	if err != nil {
		return result{}, err
	}
	return result{
		Repo: repo, Number: n, HeadSHA: pr.HeadSHA,
		AutoMerge: v.AutoMerge, Reasons: v.Reasons, Assessment: v.Assessment,
	}, nil
}

func printText(w io.Writer, r result) {
	verdict := "needs review"
	if r.AutoMerge {
		verdict = "auto-merge"
	}
	fmt.Fprintf(w, "%s#%d\t%s\n", r.Repo, r.Number, verdict)
	for _, reason := range r.Reasons {
		fmt.Fprintf(w, "\t- %s\n", reason)
	}
}
