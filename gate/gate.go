// Package gate combines the deterministic rules with Jev's answers into a
// single auto-merge verdict.
package gate

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/alam0rt/mergegate"
	"github.com/alam0rt/mergegate/judge"
	"github.com/alam0rt/mergegate/rules"
)

// Assessor is anything that can answer the judge's questions about a PR.
type Assessor interface {
	// Assess asks the built-in questions, plus extra watches, about the diff.
	Assess(ctx context.Context, pr mergegate.PullRequest, extra map[string]string) (judge.Assessment, error)
	// Ask asks only the given watches against an arbitrary state.
	Ask(ctx context.Context, state map[string]any, watches map[string]string) (map[string]float64, error)
}

// Verdict is the gate's decision. AutoMerge is false unless every check
// passed; Reasons explains the decision either way.
type Verdict struct {
	AutoMerge  bool
	Reasons    []string
	Facts      rules.Facts
	Assessment *judge.Assessment // nil when the model was not consulted
	// Tripped lists the watches that sent the PR to review, with what each
	// one read. Jev returns only a probability, so the context it was shown
	// is the only way for a reader to see why it said yes.
	Tripped []Trip
}

// Trip is a watch that vetoed a merge.
type Trip struct {
	Watch string   `json:"watch"`
	P     float64  `json:"p"`
	Limit float64  `json:"limit"`
	Read  []string `json:"read,omitempty"` // context shown to Jev, one line per item
}

// bumpKinds are the change kinds that may merge through the version-bump path.
var bumpKinds = map[string]bool{"dependency_patch": true, "dependency_minor": true}

// Evaluate decides whether pr can merge without review. The model is only
// consulted when the deterministic rules cannot decide on their own.
//
// Only the diff can approve a merge. Comments and history are read solely
// by watches, which can only veto, and each distinct set of sources is
// asked in its own request so text from one cannot sway another.
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
	case len(facts.Held) > 0:
		return human("on hold: %s", strings.Join(facts.Held, "; "))
	case len(facts.Protected) > 0:
		return human("touches protected paths: %s", strings.Join(facts.Protected, ", "))
	case facts.DocsOnly:
		v.AutoMerge = true
		return human("all %d files are documentation paths", len(pr.Files))
	case len(facts.Unreadable) > 0:
		return human("no diff available for: %s", strings.Join(facts.Unreadable, ", "))
	case facts.TooLarge:
		return human("%d changed lines exceeds max_changed_lines %d", pr.ChangedLines(), cfg.MaxChangedLines)
	case len(facts.RiskyVersions) > 0:
		return human("version change needs review: %s", strings.Join(facts.RiskyVersions, "; "))
	}

	// Split applicable watches: plain ones ride along with the built-in
	// questions; ones that read context are grouped by the sources they use.
	var plain []rules.Watch
	groups := map[string][]rules.Watch{}
	for _, w := range cfg.Watches {
		if !w.Applies(pr) {
			continue
		}
		if len(w.Uses) == 0 {
			plain = append(plain, w)
			continue
		}
		if key := sourceKey(cfg, w); key != "" {
			groups[key] = append(groups[key], w)
		}
	}

	a, err := assessor.Assess(ctx, pr, questions(plain))
	if err != nil {
		return v, err
	}
	v.Assessment = &a
	v.AutoMerge, v.Reasons = decide(cfg.Thresholds, a)
	diffPassed := v.AutoMerge
	var quiet []string
	veto(cfg, plain, a.Watches, nil, &v, &quiet)

	// Context calls cost money and can only veto, so skip them once the
	// answer is already "review".
	if v.AutoMerge {
		comments := rules.TrustedComments(cfg, pr)
		for _, key := range slices.Sorted(maps.Keys(groups)) {
			var cs []mergegate.Comment
			var hs []mergegate.Commit
			for _, s := range strings.Split(key, "+") {
				switch s {
				case rules.SourceComments:
					cs = comments
				case rules.SourceHistory:
					hs = pr.History
				}
			}
			if len(cs) == 0 && len(hs) == 0 {
				continue // nothing to read, so nothing to find
			}
			if len(cs) == 0 {
				cs = nil
			}
			if len(hs) == 0 {
				hs = nil
			}
			answers, err := assessor.Ask(ctx, judge.ContextState(pr, cs, hs), questions(groups[key]))
			if err != nil {
				v.AutoMerge = false
				return v, err
			}
			if a.Watches == nil {
				a.Watches = map[string]float64{}
			}
			for id, p := range answers {
				a.Watches[id] = p
			}
			veto(cfg, groups[key], answers, evidence(cs, hs), &v, &quiet)
		}
	}

	// A watch veto on a clean diff reads as if the diff were the problem
	// unless it says otherwise.
	if diffPassed && !v.AutoMerge {
		v.Reasons[0] = "diff alone would auto-merge: " + v.Reasons[0]
	}

	if v.AutoMerge && len(quiet) > 0 {
		v.Reasons = append(v.Reasons, "watches quiet: "+strings.Join(quiet, ", "))
	}
	return v, nil
}

// sourceKey is the sorted, enabled sources a watch reads, joined with "+".
// It is empty when every source the watch needs is turned off.
func sourceKey(cfg rules.Config, w rules.Watch) string {
	var on []string
	for _, s := range w.Uses {
		if cfg.Context.Enabled(s) && !slices.Contains(on, s) {
			on = append(on, s)
		}
	}
	slices.Sort(on)
	return strings.Join(on, "+")
}

func questions(ws []rules.Watch) map[string]string {
	if len(ws) == 0 {
		return nil
	}
	q := make(map[string]string, len(ws))
	for _, w := range ws {
		q[w.ID] = w.Question
	}
	return q
}

// veto applies watch answers to v. A watch can only turn a merge into a
// review, and a missing answer counts as tripped. read is the context the
// watches were shown, recorded on each trip.
func veto(cfg rules.Config, ws []rules.Watch, answers map[string]float64, read []string, v *Verdict, quiet *[]string) {
	for _, w := range ws {
		p, ok := answers[w.ID]
		limit := w.Limit(cfg.Thresholds)
		switch {
		case !ok:
			v.AutoMerge = false
			v.Reasons = append(v.Reasons, fmt.Sprintf("watch %s: no answer", w.ID))
		case p > limit:
			v.AutoMerge = false
			v.Reasons = append(v.Reasons, fmt.Sprintf("watch %s tripped: p=%.2f > %.2f (%s)", w.ID, p, limit, w.Question))
			v.Tripped = append(v.Tripped, Trip{Watch: w.ID, P: p, Limit: limit, Read: read})
		default:
			*quiet = append(*quiet, fmt.Sprintf("%s=%.2f", w.ID, p))
		}
	}
}

// evidence renders context sources one line each: commits as short SHA,
// date and subject; comments as author, association and first line.
func evidence(cs []mergegate.Comment, hs []mergegate.Commit) []string {
	var out []string
	for _, c := range hs {
		out = append(out, fmt.Sprintf("commit %s %s %s", c.SHA[:min(len(c.SHA), 7)], c.Date.UTC().Format("2006-01-02"), firstLine(c.Message)))
	}
	for _, c := range cs {
		out = append(out, fmt.Sprintf("%s by %s (%s) %s: %s", c.Kind, c.Author, c.Association, c.CreatedAt.UTC().Format("2006-01-02"), firstLine(c.Body)))
	}
	return out
}

func firstLine(s string) string {
	s, _, _ = strings.Cut(strings.TrimSpace(s), "\n")
	if len(s) > 100 {
		s = s[:100] + "…"
	}
	return s
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
