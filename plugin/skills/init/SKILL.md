---
name: init
description: Set up or tune mergegate (PR auto-merge gating) in the current GitHub repository. Surveys the repo, writes a minimal .mergegate.yaml, backtests it on recent PRs, and adds the CI workflow. Use when the user asks to install, initialise, configure, set up or tune mergegate, or to auto-merge safe PRs such as dependency bumps or docs changes.
---

# Set up mergegate in this repository

mergegate decides whether a PR is safe to merge without review. Its defaults
are meant to be good with no config at all. **Your job is to find where this
repo differs from the defaults, and write only that.** A good
`.mergegate.yaml` is short, and every key in it has a comment saying why it
is there.

The two workflow templates sit next to this file, in `templates/`.

Ground rules:
- **Nothing leaves the machine without the user's say-so.** That covers
  commits, pushes, PRs, repo settings and secrets. Show what you will do,
  then do it.
- **Never loosen a protection just to get more merges.** Don't raise
  thresholds, delete default protected paths or disable default watches
  unless the user asks, or you show them concrete backtest cases that justify
  it.
- **Don't go looking for API keys in the user's files.** Ask where the
  OpenRouter key is.

## 1. Prerequisites

```sh
gh auth status
gh repo view --json nameWithOwner,visibility,defaultBranchRef,mergeCommitAllowed,squashMergeAllowed,rebaseMergeAllowed,deleteBranchOnMerge
gh api repos/{owner}/{repo} --jq .allow_auto_merge    # `gh repo view` has no field for this
go version     # needed to install mergegate; if missing, ask before installing Go
```

Install mergegate pinned to a commit, and keep that SHA for the workflow:

```sh
REF=$(gh api repos/alam0rt/mergegate/commits/main --jq .sha)
GOBIN="$PWD/.mergegate-bin" go install "github.com/alam0rt/mergegate/cmd/mergegate@$REF"
.mergegate-bin/mergegate -print-config      # the defaults you are layering over
```

Put `.mergegate-bin/` in `.git/info/exclude`, not `.gitignore`: it's a local
scratch dir, not a repo change.

The backtest needs `OPENROUTER_API_KEY`. If it isn't in the environment, ask
the user for it, or have them `export` it with `! export OPENROUTER_API_KEY=...`.

If `.mergegate.yaml` already exists, you are **tuning**: read it, keep its
intent, and change only what the survey or backtest supports.

## 2. Survey (read-only)

Answer each question and keep the evidence. Every config line you write
later should trace back to one of these.

**Who opens PRs, and how?**
```sh
gh pr list --state merged --limit 60 --json number,title,author,labels,additions,deletions
ls .github/dependabot.yml renovate.json* .github/renovate.json* 2>/dev/null
grep -rlE 'gh pr create|create-pull-request|ImageUpdateAutomation' .github/workflows clusters 2>/dev/null
```
- Bots are PRs where `author.is_bot` is true. `gh pr list` shows them as
  `app/<name>`, but mergegate compares the REST login. Get the exact value
  with `gh api repos/{owner}/{repo}/pulls/<n> --jq .user.login`, which
  returns something like `dependabot[bot]`.
- Check whether the bot PRs are **opened by a workflow using `GITHUB_TOKEN`**,
  for example `gh pr create` or `peter-evans/create-pull-request` with
  `secrets.GITHUB_TOKEN`. Such PRs trigger **no** other workflows. That
  decides the workflow shape in step 5.

**What must a human always see?** Look for paths where a bad merge is
expensive and a diff looks harmless: production overlays, infra state,
database migrations, auth or ingress config, secrets (SOPS files), Flux or
Argo system dirs, release or versioning files. Each is a candidate for
`protected_paths`, with a one-line reason. Don't add paths that bots never
touch "just in case": they already need the model to agree before merging.

**Is anything docs-like missing from `docs_paths`?** Examples: `website/`,
`site/content/**`, `examples/**/*.md`. `docs_paths` *replaces* the defaults,
so if you set it, copy the default list from `-print-config` and add to it.

**Hold labels.** Run `gh label list --limit 200`. If the repo already uses a
hold-like label that isn't in the defaults (`on-hold`, `needs-discussion`,
`do not merge yet`), add it. `hold.labels` replaces the default list, so
include the defaults too.

**Versioning style.** Many dependencies at 0.x that bump the minor for
compatible releases (golang.org/x/* in `go.mod` is the classic case) will all
go to review under `zero_minor_is_major: true`. Don't change it yet. See what
the backtest says.

**Merge mechanics.**
```sh
gh api repos/{owner}/{repo}/branches/<default>/protection   # 404 none, 403 plan doesn't allow it
gh api repos/{owner}/{repo}/rules/branches/<default>        # rulesets
```
Note which merge methods are allowed and which the repo actually uses. Look
at recent commits on the default branch: "Merge pull request #…" means merge
commits; "Title (#123)" means squash.

**Domain knowledge.** Ask the user one question: *"Is there anything that has
burned you before that a harmless-looking diff could hide? For example, an
app whose upgrades need a manual step, or a setting you never want changed
automatically."* Each answer becomes a `watch` question (see step 3).

## 3. Draft `.mergegate.yaml`

Write only deviations from the defaults, each commented. Shape:

```yaml
# Only the dependency bots auto-merge; people's PRs always get a review.
allowed_authors: ["dependabot[bot]"]

# Schema migrations look like small SQL diffs but need a human.
protected_paths: ["db/migrations/**"]

watch:
  # Upgrades of X need a manual data migration (see incident 2025-03).
  - id: x_upgrade
    question: "Does this diff change the version of X?"
    threshold: 0.5
```

Writing good watch questions:
- **One fact per question**, phrased so **yes means a human should look**.
- Ask about what the diff **shows** ("does this change the X image"), not
  about intent or risk ("is this dangerous").
- Scope with `paths:` when the question only makes sense for some files.
- Add `uses: [comments]` or `uses: [history]` only if the question needs that
  text. Those watches run in separate, isolated requests.
- If the user wants a hard rule, not a judgement, use `protected_paths` or
  `hold` instead.

Show the user the draft and the evidence behind each key before going on.

## 4. Backtest

Pick about 30 recent merged PRs: all the bot PRs from the survey plus a
handful of human ones. Total cost is under $0.01.

```sh
refs=(); for n in <numbers>; do refs+=("{owner}/{repo}#$n"); done
.mergegate-bin/mergegate -config .mergegate.yaml -json "${refs[@]}" > .mergegate-bin/backtest.json
.mergegate-bin/mergegate -config .mergegate.yaml "${refs[@]}"
```

Then:
1. **Check every auto-merge by hand.** Run `gh pr diff <n>` and confirm it
   really is a pure docs change or a patch/minor bump. A false approval
   matters far more than a false review. If you find one, add a protected
   path or a watch, re-run, and tell the user.
2. **Group the reviews by reason.** For the ones that look safe, is the
   reason a config choice (for example 0.x minors, or an author not allowed)?
   Propose a change only with the cases in hand, and let the user decide.
3. **Look at the `watches quiet:` values.** A watch sitting just under its
   limit on safe PRs will flip on a future one. Mention it.
4. Keep the caveats in mind:
   - History is read at each PR's base commit, so it's accurate.
   - Comments on merged PRs include things said *after* the merge, such as
     "this broke prod". A flag from `unresolved_concern` on those is correct.
   - Old PRs judged against today's config may name paths that no longer
     exist.

Give the user a table: PR, author, verdict, main reason. Add a one-line
summary, for example "12 of 30 would have auto-merged; all 12 checked by
hand".

## 5. Workflow

Choose one:

| Situation | Use |
|-----------|-----|
| Bot PRs are opened by a workflow with `GITHUB_TOKEN` | Append the gate to **that** workflow (below) |
| Auto-merge is allowed **and** the default branch requires status checks | `templates/mergegate-auto.yaml` |
| Anything else (no protection, e.g. a private repo on the free plan) | `templates/mergegate-wait.yaml` |

For the templates, copy to `.github/workflows/mergegate.yaml` and set:
- `MERGEGATE_REF` to the SHA from step 1;
- `MERGE_METHOD` to the method the repo uses.

Don't change the triggers or add `actions/checkout`. The workflow runs with
secrets on `pull_request_target` and must never run PR code.

For a workflow that opens PRs with `GITHUB_TOKEN`, add steps after the PR is
created or updated. Model them on `mergegate-wait.yaml`: gate, then wait for
checks on the judged SHA, then `gh pr merge --match-head-commit`. The job
must expose the PR number, and needs `contents: write`, `pull-requests:
write`, `checks: read` and `statuses: read`. Merges made with `GITHUB_TOKEN`
don't trigger workflows either. So if the repo relies on a workflow that
deletes merged branches or runs on push to the default branch, delete the
branch in the same job: `gh api -X DELETE repos/$GITHUB_REPOSITORY/git/refs/heads/$BRANCH`.

Lint what you wrote:
```sh
go run github.com/rhysd/actionlint/cmd/actionlint@latest .github/workflows/*.y*ml
```

## 6. Deliver (confirm each step with the user first)

1. **Secret.** Run `gh secret set OPENROUTER_API_KEY`, piping the value in so
   it isn't echoed. If Dependabot opens PRs, also run
   `gh secret set OPENROUTER_API_KEY --app dependabot`, because runs
   triggered by Dependabot read Dependabot secrets.
2. **Settings.** Only for `mergegate-auto.yaml`: enable auto-merge with
   `gh repo edit --enable-auto-merge` if it's off.
3. **Branch and PR.** Put `.mergegate.yaml` and the workflow on a new branch
   and open a PR. Explain in it that both take effect **after** merging:
   mergegate reads the config from the base branch, and
   `pull_request_target` runs the base branch's workflow.
4. **Summary for the user:**
   - what will now auto-merge and what won't, with numbers from the backtest;
   - the per-PR cost (about $0.0001);
   - how to stop a merge: a `hold` label, `/hold`, or requesting changes;
   - how to change the rules later: edit `.mergegate.yaml` on the default
     branch, or re-run this skill.
