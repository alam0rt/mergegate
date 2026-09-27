# mergegate

Decides whether a pull request is safe to merge without a human looking at it.
Documentation-only changes and patch/minor dependency bumps pass. Everything
else is sent for review.

```console
$ mergegate cli/cli#14378 cli/cli#14391 cli/cli#14509
cli/cli#14378	auto-merge
	- jev: dependency_patch (p(bump)=0.96, p(major)=0.03, kind confidence 0.99)
	- watches quiet: recent_revert=0.07
cli/cli#14391	needs review
	- version change needs review: go.mod: v0.22.0 -> v0.23.0 (major); go.sum: v0.22.0 -> v0.23.0 (major)
cli/cli#14509	auto-merge
	- all 1 files are documentation paths
```

It works on any GitHub repository, **with no configuration**. Out of the box it:
- respects hold labels, `/hold` comments and changes-requested reviews;
- protects CI and CODEOWNERS;
- catches major bumps and downgrades without asking a model;
- reads maintainer comments and recent history for signs of trouble.

The cases the rules can't settle go to [TypeSafe's Jev](https://docs.typesafe.ai/introduction)
model through OpenRouter's decisions API. Jev answers a fixed set of narrow
yes/no questions about the diff. That costs roughly $0.0001 per PR.

**Set it up in a repo with Claude Code:**

```
/plugin marketplace add alam0rt/mergegate
/plugin install mergegate@mergegate
/mergegate:init
```

The `init` skill surveys the repo, writes a minimal `.mergegate.yaml`,
backtests it on your recent PRs, and adds the workflow. See
[the skill](plugin/skills/init/SKILL.md).

## How a PR is judged

The steps run in order. The first one that decides wins, except that step 9
can pass the PR on to step 10.

| # | Check | Result |
|---|-------|--------|
| 1 | PR is a draft | review |
| 2 | Author is not in `allowed_authors` (if that list is set) | review |
| 3 | **On hold**: a hold label, a `/hold` comment, or a changes-requested review | review |
| 4 | Any file, or the source of a rename, matches `protected_paths` | review |
| 5 | Every file matches `docs_paths` | **auto-merge**, no model call |
| 6 | A non-docs file has no patch (binary, or too big for GitHub to render) | review |
| 7 | More than `max_changed_lines` added + removed | review |
| 8 | A version in the diff crosses a major version or goes backwards | review, no model call |
| 9 | Ask Jev about the diff (below), plus plain watches | review, or continue to 10 |
| 10 | Context watches (comments, history) | review, otherwise **auto-merge** |

Any error (API failure, bad config, missing answer) produces exit status 1. An
error never produces an auto-merge.

### Only the diff can approve

Jev sees the title and the per-file diffs. The built-in questions all go in
one request:

| ID | Type | Question (paraphrased) |
|----|------|------------------------|
| `docs_only` | noul | Only docs/comments, no effect on build/deploy/run? |
| `version_bump` | noul | Every change is a version/tag/digest update, nothing else? |
| `major_bump` | noul | Any version crosses a major boundary (incl. 0.x minors)? |
| `other_changes` | noul | Any config/command/permission/network/limit change besides versions? |
| `removal` | noul | Deletes or disables anything? |
| `kind` | choice | docs / dependency_patch / dependency_minor / dependency_major / configuration / feature / ci / refactor / other |

A PR auto-merges only when all of the following hold (defaults in brackets):

- `kind` has confidence ≥ `thresholds.confidence` [0.6].
- `other_changes` ≤ `thresholds.no` [0.1] and `removal` ≤ `thresholds.no` [0.1].
- One of these two paths matches:
  - **docs path:** `kind` = docs and `docs_only` ≥ `thresholds.yes` [0.9].
  - **bump path:** `kind` is dependency_patch or dependency_minor, `version_bump` ≥ `thresholds.yes` [0.9], and `major_bump` ≤ `thresholds.no` [0.1].

Noul answers carry no confidence of their own. Requiring the separate `kind`
choice to agree with them is the consistency check.

The PR description, comments and commit history are text other people wrote,
and anyone who can comment could aim it at the model. **They never reach the
questions that approve a merge.** Only watches (below) read them, and watches
can only send a PR to review. Each distinct set of sources is asked in its own
request, so a comment can't sway the history watch either.

## Usage

```sh
export OPENROUTER_API_KEY=...        # needed only when step 9 runs
export GITHUB_TOKEN=...              # or GH_TOKEN; falls back to `gh auth token`
go install github.com/alam0rt/mergegate/cmd/mergegate@latest
mergegate [-json] [-config FILE] [-model M] owner/repo#N|PR-URL ...
mergegate [-config FILE] -print-config   # the effective config, as YAML
```

Exit status: `0` means every PR may auto-merge, `2` means at least one needs
review, `1` means an error.

## Configuration

Every key is optional. `mergegate -print-config` prints the defaults as a
complete, loadable file. Put overrides in `.mergegate.yaml` at the repository
root. **It is read from the PR's base branch**, so a PR can't loosen the rules
it is judged by. `-config` uses a local file instead. Unknown keys are an
error, and a key you leave out keeps its default, even inside a nested block.

```yaml
docs_paths: ["**/*.md", "docs/**"]   # replaces the defaults
protected_paths: ["deploy/prod/**"]  # added to the defaults, never replaces them
max_changed_lines: 400
allowed_authors: ["renovate[bot]"]   # empty (default) = anyone
thresholds: {yes: 0.9, no: 0.1, confidence: 0.6}

versions:
  zero_minor_is_major: true          # 0.4 -> 0.5 is major, as in semver

hold:
  labels: [hold, do-not-merge, "do not merge", wip, "work in progress", blocked]
  command: /hold                     # a line of its own; /unhold clears it. "" disables
  changes_requested: true

context:
  comments:
    from: [OWNER, MEMBER, COLLABORATOR]  # author_association values that count
    max: 20                              # newest N trusted comments; 0 turns off
  history:
    commits: 10                          # recent base-branch commits; 0 turns off
    changed_files_only: true             # only commits touching this PR's files
```

The default protected paths are `.mergegate.yaml`, `.github/workflows/**`,
`.github/actions/**` and `**/CODEOWNERS`.

### Versions

For each removed line, mergegate finds the added line that replaces it (by
position within a change block, and only when the text before the version
matches). It then compares every version-like token (`v1.2.3`, `1.2`, `0.29.1-rc1`).
A major bump or a downgrade goes straight to review. This covers image tags,
`go.mod`/`go.sum`, lockfiles and manifests alike. Tags that aren't semver,
like `1961-96d39adbc812`, are left to the model.

Some ecosystems bump 0.x minors for compatible releases, golang.org/x/* for
example. Set `zero_minor_is_major: false` if every such bump is going to
review for you.

### Hold

Hold signals are checked before anything can approve, docs-only included. Only
comments and reviews whose author's association is in `context.comments.from`
count, so a stranger can't `/hold` your PRs, and can't `/unhold` them either.

### Watches: your own questions

A watch is a yes/no question for Jev, phrased so that **yes means a human
should look**. Two ship by default:

| ID | Reads | Asks | Limit |
|----|-------|------|-------|
| `unresolved_concern` | comments | Did a maintainer raise a concern or ask to wait, still unresolved? | 0.3 |
| `recent_revert` | history | Was something this PR changes recently reverted, rolled back or pinned? | 0.3 |

Add your own for knowledge the built-in questions can't have: an app whose
upgrades need a manual step, a setting you've been burned by, and so on.

```yaml
watch:
  - id: ingress_snippets              # [a-z][a-z0-9_]*, unique
    question: "Does this diff add or enable nginx configuration-snippet or server-snippet annotations?"
  - id: headscale
    question: "Does this diff change the headscale container image or its version?"
    paths: ["clusters/**"]            # optional: ask only when a changed file matches
    threshold: 0.5                    # optional: review if p(yes) > this; default thresholds.no
  - id: migration_discussed
    question: "Has anyone said this upgrade needs a manual migration step?"
    uses: [comments, history]         # optional: context the question needs
  - id: recent_revert
    disabled: true                    # turn off a default watch
```

- A watch can only **veto**. A PR that failed a built-in check stays in review
  whatever its watches say, and a watch with no answer counts as tripped.
- Entries merge with the defaults by `id`: the same `id` replaces the default,
  and `disabled: true` removes it.
- A watch without `uses` rides along in the diff request. One with `uses` gets
  its own request per distinct set of sources, and is skipped when those
  sources are empty. So the default watches cost nothing on a PR nobody has
  commented on. Context watches run only while the PR is still heading for a
  merge, since they can't change a review.
- Watches apply only to PRs that reach step 9. For a hard rule, use
  `protected_paths` or `hold`.
- Ask about one thing per watch, and phrase it about what the text *shows*
  ("does this change X"), not about intent or future risk.
- Passing watches are listed with their values (`watches quiet:
  recent_revert=0.07`), so you can see how close each one came.

## Running it in CI

The `init` skill picks and fills in one of these for you.

**PRs from Dependabot, Renovate or people.** Use
[`mergegate-auto.yaml`](plugin/skills/init/templates/mergegate-auto.yaml) when
the repo allows auto-merge with branch protection. Otherwise use
[`mergegate-wait.yaml`](plugin/skills/init/templates/mergegate-wait.yaml),
which waits for the other checks on the head commit and then merges that exact
commit. Both run on `pull_request_target`, so the workflow and config always
come from the base branch. **Neither checks out PR code**, and mergegate reads
the PR only through the API. They re-run on labels, reviews and `/hold`
comments. When the gate says review, they cancel any pending auto-merge.

**PRs opened by a workflow with `GITHUB_TOKEN`** (Flux image automation, for
example) trigger no other workflows. Run the gate at the end of the job that
opens the PR, and delete the branch after merging, because
`delete-branch-on-merge`-style workflows won't fire either.

Install the binary rather than using `go run`: `go run` reports any non-zero
exit as 1, so "needs review" (2) would look like an error.

## Limits

- The diff itself is untrusted input. A patch can contain text aimed at the
  model. The path rules, hold, version check and thresholds are the defence.
  For anything that matters, set `allowed_authors` to your bots.
- Tags that aren't semver make Jev unsure. Those PRs go to review, which is
  the safe way to fail.
- PR titles are shown to the model. When a title disagrees with the diff, the
  diff wins, but a misleading title is not a check by itself.
- When backtesting merged PRs, history is read at each PR's base commit, but
  comments include anything said after the merge.
- The 0.3 limit for the default context watches was tuned on a few dozen PRs.
  Check the `watches quiet:` values on your own backtest.

## Results

Run on 2026-09-27 against `typesafe/jev-1.13-20260917`.

**cli/cli, 15 recent PRs, no configuration:**
- 4 auto-merge: two patch bumps and two docs-only PRs.
- 3 review for touching `.github/workflows`.
- 5 review as golang.org/x 0.x minor bumps. On an earlier run, before the
  version check existed, Jev let three of those five through and held back
  two.
- 3 review: a feature, a 774-line change, and one bump where Jev wasn't sure.

**A private Flux repo, 27 image-automation and hand-written PRs, with an
`allowed_authors` + `protected_paths` config:**
- 5 auto-merge. Every one was a pure version bump on inspection.
- The version check caught a 0.x jump (0.14 → 0.19, in two PRs) without
  asking the model.
- `recent_revert` flagged one plugin bump. It landed right after related
  workarounds in the same file were reverted, which is worth a look. It
  cleared the rest, the highest scoring 0.29.
- No PR that changed configuration was passed as safe.
