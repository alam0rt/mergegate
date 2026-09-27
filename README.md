# mergegate

Decides whether a pull request is safe to merge without a human looking at it.
Documentation-only changes and patch/minor dependency bumps pass. Everything
else is sent for review.

```console
$ mergegate example/gitops#219 example/gitops#199
example/gitops#219	auto-merge
	- jev: dependency_patch (p(bump)=0.95, p(major)=0.03, kind confidence 0.99)
example/gitops#199	needs review
	- p(crosses a major version) = 0.11 > 0.10
```

It works on any GitHub repository. Path rules settle the easy cases. Anything
they can't settle goes to [TypeSafe's Jev](https://docs.typesafe.ai/introduction)
model through OpenRouter's decisions API, which answers a fixed set of narrow
yes/no questions about the diff. The cost is roughly $0.0001 per PR.

## How a PR is judged

The steps run in order. The first one that decides wins.

| # | Check | Result |
|---|-------|--------|
| 1 | PR is a draft | review |
| 2 | Author is not in `allowed_authors` (if that list is set) | review |
| 3 | Any file, or the source of a rename, matches `protected_paths` | review |
| 4 | Every file matches `docs_paths` | **auto-merge**, no model call |
| 5 | A non-docs file has no patch (binary, or too big for GitHub to render) | review |
| 6 | More than `max_changed_lines` added + removed | review |
| 7 | Ask Jev (below) | review, or continue to 8 |
| 8 | Any `watch` question (see [Watches](#watches-your-own-questions)) answers yes | review, otherwise **auto-merge** |

Jev gets the title and the per-file diffs. It never sees the PR description,
which is free text that states intent rather than showing the change. All six
questions go in one request, and each is answered independently:

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

Any error (API failure, bad config, missing answer) produces exit status 1. An
error never produces an auto-merge.

## Usage

```sh
export OPENROUTER_API_KEY=...        # needed only when step 7 runs
export GITHUB_TOKEN=...              # or GH_TOKEN; falls back to `gh auth token`
go install github.com/alam0rt/mergegate/cmd/mergegate@latest
mergegate [-json] [-config FILE] [-model M] owner/repo#N|PR-URL ...
```

Exit status: `0` means every PR may auto-merge, `2` means at least one needs
review, `1` means an error.

## Configuration

Put `.mergegate.yaml` at the repository root. **It is read from the PR's base
branch**, so a PR can't loosen the rules it is judged by. `-config` overrides
it with a local file. Unknown keys are an error.

```yaml
docs_paths: ["**/*.md", "docs/**"]   # replaces the defaults
protected_paths: ["deploy/prod/**"]  # added to the defaults, never replaces them
max_changed_lines: 400
allowed_authors: ["renovate[bot]"]   # empty = anyone
thresholds: {yes: 0.9, no: 0.1, confidence: 0.6}
```

### Watches: your own questions

A watch is an extra yes/no question for Jev, phrased so that **yes means a human
should look**. Use it for domain knowledge the built-in questions can't have:
an app whose upgrades need a manual step, a setting you've been burned by, and
so on.

```yaml
watch:
  - id: ingress_snippets              # [a-z][a-z0-9_]*, unique
    question: "Does this diff add or enable nginx configuration-snippet or server-snippet annotations?"
  - id: headscale
    question: "Does this diff change the headscale container image or its version?"
    paths: ["clusters/**"]            # optional: ask only when a changed file matches
    threshold: 0.5                    # optional: review if p(yes) > this; default thresholds.no
```

- A watch can only **veto**. A PR that fails a built-in check stays in review
  however its watches answer, and a watch with no answer counts as tripped.
- Watches go out in the same request as the built-in questions, and each is
  answered on its own. Each one adds roughly $0.00002 per PR.
- They apply only to PRs that reach the model. Docs-only and protected-path
  PRs are settled by path first. For a hard rule, use `protected_paths`.
- Ask about one thing per watch, and phrase it about what the diff *shows*
  ("does this change X"), not about intent or future risk.
- Passing watches are listed with their values (`watches quiet:
  headscale=0.02`), so you can see how close each one came.

Default protected paths are `.mergegate.yaml`, `.github/workflows/**`,
`.github/actions/**` and `**/CODEOWNERS`. See
[`examples/flux.mergegate.yaml`](examples/flux.mergegate.yaml) for a Flux repo.

## Running it in CI

PRs opened with `GITHUB_TOKEN` don't trigger `pull_request` workflows. For
bot-opened PRs, run the gate in the same job that opens the PR:

```yaml
      - name: Auto-merge if the gate passes
        env:
          GH_TOKEN: ${{ secrets.GITHUB_TOKEN }}
          OPENROUTER_API_KEY: ${{ secrets.OPENROUTER_API_KEY }}
        run: |
          GOBIN="$RUNNER_TEMP/bin" go install github.com/alam0rt/mergegate/cmd/mergegate@<commit>
          if "$RUNNER_TEMP/bin/mergegate" "$GITHUB_REPOSITORY#$PR"; then
            gh pr merge "$PR" --auto --squash
          fi
```

Install the tool and run the binary. `go run` reports any non-zero exit as 1,
so "needs review" (2) would look like an error.

Without branch protection (for example a private repo on the free plan),
`--auto` is unavailable. Wait for your checks on the head commit yourself, then
run `gh pr merge --match-head-commit <sha>`.

`--auto` still waits for required status checks, so the gate adds to branch
protection and doesn't replace it.

## Limits

- The diff itself is untrusted input. A patch can contain text aimed at the
  model. The path rules, `allowed_authors` and the thresholds are the defence.
  For anything that matters, set `allowed_authors` to your bots.
- Tags that aren't semver (for example `1961-96d39adbc812`) make
  Jev unsure. Those PRs go to review, which is the safe way to fail.
- PR titles are shown to the model. When a title disagrees with the diff, the
  diff wins, but a misleading title is not a check by itself.

## Results on a Flux repo

27 recent image-automation and hand-written PRs, run on 2026-09-27 against
`typesafe/jev-1.13-20260917`. Total cost was $0.0019 for 25 model calls.

- 11 auto-merge. Every one was a pure version bump on inspection.
- 16 review:
  - 5 config/feature changes, all correct.
  - 3 real risk signals, all correct: a 0.x minor bump (breaking under
    semver) and two large version jumps.
  - 1 unrenderable diff (a Flux self-upgrade).
  - 1 draft.
  - 6 were safe but judged uncertain: non-semver build tags, multi-image
    updates, one Flux upgrade.
- No PR that changed configuration was passed as safe.
