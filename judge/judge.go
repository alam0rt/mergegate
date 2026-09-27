// Package judge asks TypeSafe's Jev model, via OpenRouter's decisions API,
// a fixed set of narrow questions about a pull request's diff.
package judge

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"time"

	openrouter "github.com/OpenRouterTeam/go-sdk"
	"github.com/OpenRouterTeam/go-sdk/models/components"
	"github.com/OpenRouterTeam/go-sdk/models/operations"
	"github.com/OpenRouterTeam/go-sdk/retry"
	"github.com/alam0rt/mergegate"
)

// DefaultModel is the Jev model used unless overridden.
const DefaultModel = "typesafe/jev-1.13"

// Question IDs. Every question is answered independently against the same
// state, so each one asks about exactly one property of the diff.
const (
	QDocsOnly     = "docs_only"
	QVersionBump  = "version_bump"
	QMajorBump    = "major_bump"
	QOtherChanges = "other_changes"
	QRemoval      = "removal"
	QKind         = "kind"
)

var nouls = map[string]string{
	QDocsOnly: "Does this diff change only documentation, comments, or other prose, " +
		"with no effect on how anything is built, deployed, configured, or run?",
	QVersionBump: "Is every change in this diff an update to the version, tag, or digest " +
		"of an existing dependency, container image, or chart, with nothing else changed?",
	QMajorBump: "Does any version change in this diff cross a major version boundary " +
		"(for example 1.x to 2.x, or 0.4 to 0.5 for a 0.x version), or jump by an unusually large amount?",
	QOtherChanges: "Apart from version, tag, or digest updates, does this diff change any " +
		"configuration value, command, permission, network setting, resource limit, or code?",
	QRemoval: "Does this diff delete or disable any resource, service, file, data, or setting?",
}

// Kinds are the options for the change-kind choice.
var Kinds = map[string]string{
	"docs":             "documentation or comments only",
	"dependency_patch": "a patch-level version bump of an existing dependency or image",
	"dependency_minor": "a minor version bump of an existing dependency or image",
	"dependency_major": "a major version bump of an existing dependency or image",
	"configuration":    "a change to configuration values or settings",
	"feature":          "new functionality, resources, or services",
	"ci":               "changes to CI, build, or automation tooling",
	"refactor":         "restructuring with no intended behaviour change",
	"other":            "anything else",
}

// Assessment is Jev's answers about one pull request. The five float fields
// are probabilities that the corresponding question is true.
type Assessment struct {
	DocsOnly     float64
	VersionBump  float64
	MajorBump    float64
	OtherChanges float64
	Removal      float64

	Kind              string
	KindConfidence    float64
	KindProbabilities map[string]float64

	// Watches holds p(yes) for each repo-specific watch question, by watch ID.
	Watches map[string]float64

	Model string
	Cost  float64
}

// Judge assesses pull requests with Jev.
type Judge struct {
	sdk   *openrouter.OpenRouter
	model string
	// callOpts are passed on every request. The decisions endpoint has its
	// own server list and ignores openrouter.WithServerURL, so a base-URL
	// override has to go here.
	callOpts []operations.Option
}

type options struct {
	model     string
	serverURL string
	retries   bool
}

// Option configures a Judge.
type Option func(*options)

// WithModel overrides DefaultModel.
func WithModel(m string) Option { return func(o *options) { o.model = m } }

// WithServerURL points the client at a different OpenRouter base URL.
func WithServerURL(u string) Option { return func(o *options) { o.serverURL = u } }

// WithRetries enables or disables retrying failed requests (on by default).
func WithRetries(on bool) Option { return func(o *options) { o.retries = on } }

// New returns a Judge authenticated with an OpenRouter API key.
func New(apiKey string, opts ...Option) *Judge {
	o := options{model: DefaultModel, retries: true}
	for _, opt := range opts {
		opt(&o)
	}
	rc := retry.Config{Strategy: "none"}
	if o.retries {
		rc = retry.Config{
			Strategy: "backoff",
			Backoff: &retry.BackoffStrategy{
				InitialInterval: 500, MaxInterval: 5000, Exponent: 2, MaxElapsedTime: 30000,
			},
			RetryConnectionErrors: true,
		}
	}
	sdkOpts := []openrouter.SDKOption{
		openrouter.WithSecurity(apiKey),
		openrouter.WithXTitle("mergegate"),
		openrouter.WithRetryConfig(rc),
		openrouter.WithTimeout(60 * time.Second),
	}
	j := &Judge{sdk: openrouter.New(sdkOpts...), model: o.model}
	if o.serverURL != "" {
		j.callOpts = append(j.callOpts, operations.WithServerURL(o.serverURL))
	}
	return j
}

// State is what Jev sees: the title and the diff, never the PR body.
func State(pr mergegate.PullRequest) map[string]any {
	files := make([]any, 0, len(pr.Files))
	for _, f := range pr.Files {
		m := map[string]any{
			"path":      f.Path,
			"status":    f.Status,
			"additions": f.Additions,
			"deletions": f.Deletions,
			"patch":     f.Patch,
		}
		if f.PreviousPath != "" {
			m["previous_path"] = f.PreviousPath
		}
		files = append(files, m)
	}
	return map[string]any{
		"repository": pr.Repo,
		"title":      pr.Title,
		"files":      files,
	}
}

// watchPrefix namespaces watch IDs so they cannot collide with built-in questions.
const watchPrefix = "watch_"

func questions(extra map[string]string) map[string]components.Questions {
	qs := make(map[string]components.Questions, len(nouls)+len(extra)+1)
	all := make(map[string]string, len(nouls)+len(extra))
	for id, text := range nouls {
		all[id] = text
	}
	for id, text := range extra {
		all[watchPrefix+id] = text
	}
	for id, text := range all {
		qs[id] = components.CreateQuestionsNoul(components.DecisionsNoulQuestion{
			Instructions: components.CreateDecisionsNoulQuestionInstructionsStr(text),
		})
	}
	criteria := make(map[string]*components.Criteria, len(Kinds))
	for k, desc := range Kinds {
		c := components.CreateCriteriaStr(desc)
		criteria[k] = &c
	}
	qs[QKind] = components.CreateQuestionsChoice(components.DecisionsChoiceQuestion{
		Instructions: components.CreateDecisionsChoiceQuestionInstructionsStr("What kind of change is this diff?"),
		Criteria:     criteria,
	})
	return qs
}

// Assess asks Jev every question about pr in a single request. extra maps
// watch IDs to additional yes/no questions; their answers land in
// Assessment.Watches.
func (j *Judge) Assess(ctx context.Context, pr mergegate.PullRequest, extra map[string]string) (Assessment, error) {
	resp, err := j.sdk.Alpha.Decisions.Create(ctx, components.DecisionsRequest{
		Model:     j.model,
		State:     components.CreateStateMapOfAny(State(pr)),
		Questions: questions(extra),
	}, j.callOpts...)
	if err != nil {
		return Assessment{}, fmt.Errorf("jev decisions: %w", err)
	}

	a := Assessment{Model: resp.Model}
	if resp.Usage.Cost != nil {
		a.Cost = *resp.Usage.Cost
	}
	// A slice, not a map, so a missing answer is always reported the same way.
	targets := []struct {
		id  string
		dst *float64
	}{
		{QDocsOnly, &a.DocsOnly}, {QVersionBump, &a.VersionBump}, {QMajorBump, &a.MajorBump},
		{QOtherChanges, &a.OtherChanges}, {QRemoval, &a.Removal},
	}
	for _, t := range targets {
		p, err := noul(resp.Answers, t.id)
		if err != nil {
			return Assessment{}, err
		}
		*t.dst = p
	}
	if len(extra) > 0 {
		a.Watches = make(map[string]float64, len(extra))
	}
	for _, id := range slices.Sorted(maps.Keys(extra)) {
		p, err := noul(resp.Answers, watchPrefix+id)
		if err != nil {
			return Assessment{}, err
		}
		a.Watches[id] = p
	}

	kind, ok := resp.Answers[QKind]
	if !ok {
		return Assessment{}, fmt.Errorf("jev returned no answer for %q", QKind)
	}
	if kind.DecisionsChoiceAnswer == nil || kind.Type != components.AnswersTypeChoice {
		return Assessment{}, fmt.Errorf("jev answered %q with type %q, want choice", QKind, kind.Type)
	}
	a.Kind = kind.DecisionsChoiceAnswer.Choice
	a.KindProbabilities = kind.DecisionsChoiceAnswer.Probabilities
	if c := kind.DecisionsChoiceAnswer.Confidence; c != nil {
		a.KindConfidence = *c
	}
	return a, nil
}

// noul returns the probability from the noul answer to question id.
func noul(answers map[string]components.Answers, id string) (float64, error) {
	ans, ok := answers[id]
	if !ok {
		return 0, fmt.Errorf("jev returned no answer for %q", id)
	}
	if ans.DecisionsNoulAnswer == nil || ans.Type != components.AnswersTypeNoul {
		return 0, fmt.Errorf("jev answered %q with type %q, want noul", id, ans.Type)
	}
	return ans.DecisionsNoulAnswer.Noul, nil
}
