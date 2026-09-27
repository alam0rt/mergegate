// Package gate combines the deterministic rules with Jev's answers into a
// single auto-merge verdict.
package gate

import (
	"context"
	"fmt"
	"strings"

	"github.com/alam0rt/mergegate"
	"github.com/alam0rt/mergegate/judge"
	"github.com/alam0rt/mergegate/rules"
)

// Assessor is anything that can answer the judge's questions about a PR.
type Assessor interface {
	Assess(ctx context.Context, pr mergegate.PullRequest, extra map[string]string) (judge.Assessment, error)
}

// Verdict is the gate's decision. AutoMerge is false unless every check
// passed; Reasons explains the decision either way.
type Verdict struct {
	AutoMerge  bool
	Reasons    []string
	Facts      rules.Facts
	Assessment *judge.Assessment // nil when the model was not consulted
}

// bumpKinds are the change kinds that may merge through the version-bump path.
var bumpKinds = map[string]bool{"dependency_patch": true, "dependency_minor": true}

// Evaluate decides whether pr can merge without review. The model is only
// consulted when the deterministic rules cannot decide on their own.
func Evaluate(ctx context.Context, cfg rules.Config, pr mergegate.PullRequest, assessor Assessor) (Verdict, error) {
	facts := rules.Check(cfg, pr)
	v := Verdict{Facts: facts}
	human := func(format string, args ...any) (Verdict, error) {
		v.Reasons = append(v.Reasons, fmt.Sprintf(format, args...))
		return v, nil
	}

	switch {
	case pr.Draft:
		return human("pull request is a draft")
	case !facts.AuthorAllowed:
		return human("author %s is not in allowed_authors", pr.Author)
	case len(facts.Protected) > 0:
		return human("touches protected paths: %s", strings.Join(facts.Protected, ", "))
	case facts.DocsOnly:
		v.AutoMerge = true
		return human("all %d files are documentation paths", len(pr.Files))
	case len(facts.Unreadable) > 0:
		return human("no diff available for: %s", strings.Join(facts.Unreadable, ", "))
	case facts.TooLarge:
		return human("%d changed lines exceeds max_changed_lines %d", pr.ChangedLines(), cfg.MaxChangedLines)
	}

	var watches []rules.Watch
	var extra map[string]string
	for _, w := range cfg.Watches {
		if w.Applies(pr) {
			watches = append(watches, w)
			if extra == nil {
				extra = map[string]string{}
			}
			extra[w.ID] = w.Question
		}
	}

	a, err := assessor.Assess(ctx, pr, extra)
	if err != nil {
		return v, err
	}
	v.Assessment = &a
	v.AutoMerge, v.Reasons = decide(cfg.Thresholds, a)

	// Watches run after decide and can only veto: a quiet watch never
	// rescues a PR that failed a built-in check.
	var quiet []string
	for _, w := range watches {
		p, ok := a.Watches[w.ID]
		switch {
		case !ok:
			v.AutoMerge = false
			v.Reasons = append(v.Reasons, fmt.Sprintf("watch %s: no answer", w.ID))
		case p > w.Limit(cfg.Thresholds):
			v.AutoMerge = false
			v.Reasons = append(v.Reasons, fmt.Sprintf("watch %s: p=%.2f > %.2f (%s)", w.ID, p, w.Limit(cfg.Thresholds), w.Question))
		default:
			quiet = append(quiet, fmt.Sprintf("%s=%.2f", w.ID, p))
		}
	}
	if v.AutoMerge && len(quiet) > 0 {
		v.Reasons = append(v.Reasons, "watches quiet: "+strings.Join(quiet, ", "))
	}
	return v, nil
}

// decide applies the thresholds to Jev's answers. There are two ways to
// merge: a documentation-only change, or a patch/minor version bump with
// nothing else in it. Either way the free-text choice has to agree with the
// yes/no questions, and be confident, before the gate trusts them.
func decide(t rules.Thresholds, a judge.Assessment) (bool, []string) {
	var fails []string
	check := func(ok bool, format string, args ...any) {
		if !ok {
			fails = append(fails, fmt.Sprintf(format, args...))
		}
	}

	check(a.KindConfidence >= t.Confidence, "change kind %s has confidence %.2f < %.2f", a.Kind, a.KindConfidence, t.Confidence)
	check(a.OtherChanges <= t.No, "p(other changes besides versions) = %.2f > %.2f", a.OtherChanges, t.No)
	check(a.Removal <= t.No, "p(removes something) = %.2f > %.2f", a.Removal, t.No)

	switch {
	case a.Kind == "docs":
		check(a.DocsOnly >= t.Yes, "p(documentation only) = %.2f < %.2f", a.DocsOnly, t.Yes)
		if len(fails) == 0 {
			return true, []string{fmt.Sprintf("jev: documentation only (p=%.2f, kind confidence %.2f)", a.DocsOnly, a.KindConfidence)}
		}
	case bumpKinds[a.Kind]:
		check(a.VersionBump >= t.Yes, "p(only a version bump) = %.2f < %.2f", a.VersionBump, t.Yes)
		check(a.MajorBump <= t.No, "p(crosses a major version) = %.2f > %.2f", a.MajorBump, t.No)
		if len(fails) == 0 {
			return true, []string{fmt.Sprintf("jev: %s (p(bump)=%.2f, p(major)=%.2f, kind confidence %.2f)",
				a.Kind, a.VersionBump, a.MajorBump, a.KindConfidence)}
		}
	default:
		fails = append(fails, fmt.Sprintf("change kind %s is not auto-mergeable", a.Kind))
	}
	return false, fails
}
