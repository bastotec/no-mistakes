---
title: CLI Commands
description: Complete reference for all no-mistakes commands and flags.
---

## no-mistakes

Attach to the active pipeline run for the current branch when one exists. If none exists, bare `no-mistakes` can start the setup wizard to create a branch, commit changes, push through the gate, wait for the daemon to register the new run, and then attach. If the push succeeds but no run is registered, that wizard path now exits with an explicit error instead of silently falling through. By default this wizard path is interactive and only runs in a TTY session. In non-interactive contexts, bare `no-mistakes` falls back to showing the last 5 runs inline unless you pass `-y` or `--yes` to run the wizard and accept defaults automatically. When a TTY is available, `-y` keeps the wizard visible, shows a brief `waiting for run…` state after push, and auto-advances the default path; without a TTY it falls back to the headless path.

```sh
no-mistakes
no-mistakes --skip test,lint
```

| Flag          | Type     | Default | Description                                          |
| ------------- | -------- | ------- | ---------------------------------------------------- |
| `-y`, `--yes` | `bool`   | `false` | Run setup wizard and accept defaults automatically   |
| `--skip`      | `string` | (none)  | Comma-separated pipeline steps to skip for a new run |

Unlike `no-mistakes attach`, bare `no-mistakes` only auto-attaches to an active run on the current branch.
`--skip` only applies when bare `no-mistakes` starts a new pipeline run through the wizard; it does not skip a step on an already-active run.
The only valid `--skip` step names are `intent`, `rebase`, `review`, `jev`, `test`, `document`, `lint`, `push`, `pr`, and `ci`. Repository gate names are refused.

## no-mistakes init

Initialize or refresh the gate for the current repository.

`init` requires an `origin` remote to identify the upstream repository: later pipeline steps push validated branches to the configured target and open pull requests against that upstream. If `origin` is missing, add it with `git remote add origin <url>`, replacing `<url>` with the upstream repository's URL, then re-run `init`.

```sh
no-mistakes init
no-mistakes init --fork-url git@github.com:you/my-repo.git
no-mistakes init --worktree-root ~/work/my-repo-runs
```

| Flag              | Type     | Default | Description                                                                                      |
| ----------------- | -------- | ------- | ------------------------------------------------------------------------------------------------ |
| `--fork-url`      | `string` | (none)  | GitHub fork remote URL to push branches to while opening PRs against `origin`                  |
| `--worktree-root` | `string` | (none)  | Directory to create this repository's run worktrees in; prints the `worktree_roots` entry to add |

Creates or refreshes a local bare repo, installs the managed pre-receive admission and post-receive notification hooks, best-effort isolates the gate repo's hook path from shared git config changes when Git supports `config --worktree`, adds or repairs the `no-mistakes` git remote, detects the default branch, records or updates the repo in SQLite, installs the `/no-mistakes` agent skill at user level into `~/.claude/skills/no-mistakes/SKILL.md` and `~/.agents/skills/no-mistakes/SKILL.md`, and ensures the daemon is running, installing the managed service when available and falling back to a detached daemon otherwise.
`init` writes no skill files into the repo; the user-level copies serve Claude Code (`~/.claude/skills`) and agents that use the vendor-neutral `~/.agents/skills` convention (Codex, OpenCode, Rovo Dev, and Pi) across all repos. Grok Build is a pipeline runner and does not consume this installed skill.
If the home `.claude` links to `.agents`, `.claude/skills` links to `.agents/skills`, or the reverse, `init` follows that layout and still makes the skill readable from both logical paths.
If the repo still contains a vendored skill copy written by an older no-mistakes version, `init` leaves it untouched and prints a notice that it is no longer needed and can be removed.
The gate advertises Git push-option support, so you can skip steps for one push with `git push -o no-mistakes.skip=test,lint no-mistakes <branch>`.

For GitHub fork contributions, keep `origin` pointed at the parent repository and pass `--fork-url` with your fork remote URL.
The Push step, rebase branch-sync, and CI repair publication use the fork, including when [`ci.revalidate_repairs`](/no-mistakes/reference/repo-config/#cirevalidate_repairs) sends a repair back through Push, while GitHub PR and CI commands stay scoped to the parent repository and create PRs with `--head <fork-owner>:<branch>`.
Fork routing currently requires both `origin` and `--fork-url` to be GitHub remotes with owner/repo paths.

`--worktree-root` is for directory-scoped toolchain configuration (mise, direnv), which resolves by path ancestry and so never reaches a run worktree under `NM_HOME`.
The flag resolves the directory, then prints the [`worktree_roots`](/no-mistakes/reference/global-config/#worktree_roots) entry to add to `~/.no-mistakes/config.yaml`; the global config is hand-maintained, so `init` never rewrites it for you.
When the file already has a `worktree_roots:` block, `init` prints just the entry line to add under it - a second `worktree_roots:` key would make the config unparseable and stop the daemon.
Runs are created at `<dir>/<run id>` once the entry is in place; no-mistakes only ever touches the directories its own run records name, and everything else in that directory is left alone.
`init` rejects the directories the daemon would refuse to start on, so the entry it prints is always one you can paste: a directory inside `NM_HOME`, inside the repository being initialized or any other gated checkout, already used by another checkout (it names that checkout), or that exists as a non-directory.

Two refusals apply to every `init`, with or without the flag.
It refuses to register a checkout that contains a directory an existing [`worktree_roots`](/no-mistakes/reference/global-config/#worktree_roots) entry points at, naming that entry, because registering it is what would make the placement unusable and stop the daemon; place the checkout elsewhere or repoint the entry first.
It also refuses to register anything while `~/.no-mistakes/config.yaml` does not load, naming the fault, because the daemon refuses to start on that same config.

Re-running `init` on an already-initialized repo succeeds and reports `Gate already initialized (refreshed)`.
It refreshes managed gate wiring, origin/default-branch metadata, hook-path isolation, and the installed agent skill, overwriting any stale `SKILL.md` content from an older binary.
When a fork URL is already recorded, re-running `init` without `--fork-url` preserves it.
Passing `--fork-url` again replaces the stored fork URL after validation.
If you rename or move an initialized working directory and the old path no longer exists, re-running `init` from the new path reattaches the existing gate, preserves the repo ID and run history, and updates the stored working path.
If you copy an initialized working directory while the original still exists, the copy is treated as a separate repo and gets a fresh gate.
Fresh init rolls back gate setup when a required gate or daemon step fails; refresh does not eject a pre-existing gate if daemon startup fails.
Skill installation is best-effort: if the skill write fails, init reports it and leaves the working gate in place.

## no-mistakes ci-workflow

Generate `.github/workflows/ci.yml` from the repository's `.no-mistakes.yaml` commands, so GitHub Actions registers real checks that the gate's CI step can monitor.

```sh
no-mistakes ci-workflow
no-mistakes ci-workflow --force
```

| Flag            | Type   | Default | Description                      |
| --------------- | ------ | ------- | -------------------------------- |
| `-f`, `--force` | `bool` | `false` | Overwrite existing workflow file |

Run it from anywhere inside a repository with a `.no-mistakes.yaml`; the config is read from and the workflow written at the git toplevel, and `commands.test` must be configured or the command errors.
The generated workflow runs the configured test command (and lint command when `commands.lint` is set; with an empty `commands.lint` it emits a test-only workflow, since lint runs as the combined document+lint agent pass) on push to the repository's default branch, resolved from the `origin` remote and falling back to `main`, and on all pull requests.
Commands are inserted verbatim into YAML block scalars, with multi-line commands indented to stay inside the scalar.
The template is Go-focused (it uses `actions/setup-go` with `go-version-file: go.mod`); non-Go repos can adapt the generated file.
An existing `.github/workflows/ci.yml` is never overwritten without `--force`, overwrites are atomic, and the command refuses to write through a symlinked `.github`, `workflows`, or `ci.yml`.
Commit and push the generated file to enable the checks.

## no-mistakes axi

Agent eXperience Interface for non-interactive agents.
Most agent workflows use the installed `/no-mistakes` skill, which drives this command surface underneath.
It prints TOON to stdout, prints progress to stderr, and uses structured stdout errors with exit code `1` for operational failures and `2` for bad usage.
At the TOON output boundary, unsupported C0 control bytes are rendered as visible `\xNN` escapes while tabs, carriage returns, newlines, printable Unicode, and the underlying durable logs remain unchanged.
If TOON encoding still fails, AXI prints a structured error instead of returning successful empty stdout.
The calling agent drives AXI approval gates but does not replace the configured pipeline agent that performs validation.

```sh
no-mistakes axi
```

With no subcommand, shows the executable path, description, repo, current branch, daemon state, recent runs, and next-step help, including a pointer to `no-mistakes axi run --help` and the installed `/no-mistakes` skill for full driving guidance.
When the current branch has an active run, that run appears as `active_run` with any approval gate and help for `axi respond` when it is parked or `axi status` when it is still running.
If an active run object is parked at a decision gate, it includes `awaiting_agent: parked <duration>` immediately after `status`.
That field is observability only; the `gate:` object still tells the agent which response to send.
If a step is actively `running` or `fixing`, the run object can also include an `active_steps` table with step-scoped `active_for`, current-round `round_active_for`, `last_activity`, native `agent_pid` when one is currently running, and the current execution or fix round.
When only another branch has an active run, that run appears as `other_branch_active_run`; the help tells agents to leave it alone and start validation for the current branch.
AXI help and outputs always repeat the preserve-prior-gate-progress contract: after a gate round has already produced fix commits, additional fixes belong on the same branch.
When a relevant `branch_sync` object is present, they also include version-matched synchronization guidance to follow before a post-pipeline local commit or fresh run.
Agents must not abort-and-restart, reset, replace the branch, or improvise Git recovery in a way that drops prior gate-fix commits.
A fresh run re-validates the current branch state, so already-resolved findings do not re-surface.

## no-mistakes axi run

Start or reattach to validation for the current branch, blocking until the first approval gate, CI-ready decision point, or final outcome.
An active run on another branch does not block starting validation for the current branch.

```sh
no-mistakes axi run --intent "the user's goal"
no-mistakes axi run --intent "the user's goal" --skip test,lint
no-mistakes axi run --intent "the user's goal" --yes
no-mistakes axi run --intent "the user's goal" --base-branch epic/foo
```

| Flag            | Type     | Default | Description                                                                                          |
| --------------- | -------- | ------- | ---------------------------------------------------------------------------------------------------- |
| `--intent`      | `string` | (none)  | What the user set out to accomplish; required to start a new run                                     |
| `-y`, `--yes`   | `bool`   | `false` | Auto-resolve eligible gates until a decision point or outcome                                       |
| `--skip`        | `string` | (none)  | Comma-separated pipeline steps to skip                                                               |
| `--base-branch` | `string` | (none)  | Integration branch for this run only; overrides [`pr.base_branch`](/no-mistakes/reference/repo-config/#prbase_branch) |
| `--preserve-history` | `string` | (none) | Full integration-base commit SHA; requires `--base-branch`, preserves the submitted head and forbids automatic integration and forced publication |
| `--existing-pr` | `string` | (none) | Explicit existing github.com PR URL; validates repository, source ref and head; remembered for the branch; never falls back to another PR |
| `--isolate-test` | `bool` | `false` | Explicit [run-scoped Test isolation](#run-scoped-test-isolation); refuses unsupported setups |
| `--isolate` | `bool` | `false` | Explicit [offline run confinement](#offline-run-confinement) for Review, Test, Document, Lint and repository gates, including repairs; no integration or delivery |
| `--retire-existing-pr` | `bool` | `false` | Drop the branch's remembered PR association and start no run |
| `--wait`        | `duration` | `8m`    | Maximum time for active-run lookup and run driving before the caller must reattach |
| `--launch-nonce` | `string` | (none) | Non-secret correlation identifier for a durable pre-drive receipt; requires `--validation-generation` |
| `--validation-generation` | `string` | (none) | Caller-selected validation generation bound to `--launch-nonce`; requires that flag |

`--intent` is not a description of the diff.
It is the user's goal or request, and no-mistakes uses it verbatim instead of transcript inference.
Err on the side of completeness: include the goal, important decisions and tradeoffs, constraints or approaches ruled in or out, and explicit requests that might otherwise look surprising in the diff.
When starting a new run, `axi run` refuses the default branch and uncommitted working trees with actionable errors instead of auto-branching or auto-committing.
Ordinary reattachment to an in-flight run does not require `--intent`; [strict launch receipts](#strict-launch-receipts) require the original intent bytes on every retry.
`--base-branch` is persisted on the run so rebase, PR, and CI honor it after resume.
Reattaching with a `--base-branch` that differs from the active run's stored target is refused rather than silently discarded; omit the flag to reattach, or abort the active run first.
Ordinary reattachment accepts either the run's immutable submitted head or its current pipeline head, so pipeline-created fix commits do not detach an unchanged submitting worktree.
When neither identity matches, `axi run` keeps the fresh-run path but refuses a gate push while `branch_sync` says the pipeline still owns the branch.
That refusal returns the complete structured state and its `continue_active_run` or `recover_custody` next action instead of a raw Git non-fast-forward.
Reattaching to an in-flight run can proceed while the daemon is already running even if the global config file has become invalid, but starting a fresh run still requires valid global config.
Starting a fresh run also requires a runnable effective pipeline agent.
If the configured native agent or ACP runner is unavailable, the run fails before any pipeline step starts instead of reporting command-only validation as a passed gate.
With `--yes`, `axi run` treats both `action: auto-fix` and `action: ask-user` findings as standing consent for the pipeline to fix them by selecting every finding, then accepts the resulting fix review.
Gates with no findings or only `action: no-op` findings are approved as-is, and each step is fixed at most once so unresolved findings do not loop forever.
Protected-path refusals, repair-budget exhaustion, repair reconciliation after daemon recovery, and steps interrupted by orderly daemon shutdown are exceptions to this automatic handling. `--yes` leaves each parked for its explicit, gate-specific response.

A CI gate is never approved or skipped under `--yes`. Every CI-step gate is treated as not green, regardless of its findings or whether it is a fix-review gate. This includes failing checks, cancelled checks no rerun replaces, merge conflicts, review-bot failures, and checks awaiting approval. `--yes` may request a fix at most once per invocation, subject to the exceptions above, but otherwise leaves the gate parked. Approving or skipping requires an explicit operator response without `--yes`; approval over still-failing checks is recorded as `passed-with-override`.

Without `--yes`, an agent driving `axi run` should stop when a gate contains `action: ask-user` findings and relay each finding's ID, file, and full description to the user before responding.
Review gates include a `note` field reminding agents that `auto_fix.review` defaults to `0`, so blocking and ask-user review findings park for a decision unless configuration explicitly opts back into review auto-fix.
Long-running `axi run` calls are working, not stalled; if one returns a `gate:`, read that output and answer it with `axi respond`.
`--wait` defaults to 8m so an agent harness with a 10-minute tool cap gets a structured error instead of an unbounded hang. It bounds the active-run lookup, event-subscription acknowledgement, and subsequent run driving. Elapsed wait is not a pipeline failure and does not mean the daemon is dead: run `no-mistakes axi status` and reattach with `axi run`. A live daemon that is slow to answer a pre-drive `get_active_run` or `get_run` state read is retried after a health probe rather than reported as an I/O failure or mistaken for an absent run.
Backgrounding a call is fine for an agent harness, but the run never advances past a gate on its own.
When the CI step is still monitoring an open PR and checks are green - or the trusted default-branch config declares [`no_ci: true`](/no-mistakes/reference/repo-config/#no_ci) with no registered checks - `axi run` exits successfully with `outcome: checks-passed` instead of waiting for a human merge. A generic empty check list without that declaration is not ready.
Treat that as the agent stopping point: ask the user to review and merge the PR from the `help` line.
If that PR later falls behind the default branch or hits a merge conflict, do not run `axi run`, `rerun`, or a manual rebase while the CI monitor is still running.
By default, the monitor auto-rebases onto the base, resolves actual conflicts, revalidates from Review because rebasing cannot prove continuity with the reviewed head, and re-pushes the branch through Push; a PR that is merely behind but clean needs no command. [History-preserving runs](#history-preserving-validation) refuse that integration instead.
After that monitor ends, see [`no-mistakes rerun`](#no-mistakes-rerun) for the restart conditions.
Successful outcomes (`checks-passed`, `passed`, `passed-with-override`, and `passed-with-skips`) also carry `help` instructions telling the agent to summarize the run.
`passed-with-override` is a completed run whose CI approval gate was approved by a human while a live check was still failing; it stays a success but reads distinctly from a genuinely green `passed`, and its `help` names the failure the operator approved past.
`passed-with-skips` is a completed run where PR publication or CI verification automatically skipped because its provider was unavailable, or CI had no PR URL. It retains exit code 0: missing verification is not a failing code verdict. `run.automatic_skips` names each affected step and cause, and `run.head_sha` gives the full recorded head in both drive output and `axi status`. Report that missing evidence; this outcome does not establish CI readiness or a merge. Explicit per-run skips retain their existing behavior. If the run also has a CI approval override, `passed-with-override` takes precedence and the automatic skip causes remain visible. Legacy rows without a recorded skip cause keep their prior classification; their logs remain inspectable.
When the pipeline applied fixes, they include a `fixes` table and a `help` instruction to acknowledge the misses and list those fixes for the user's review.

### History-preserving validation

For an already prepared integration whose exact history must survive validation:

```sh
no-mistakes axi run --intent "the user's goal" \
  --base-branch main --preserve-history <full-base-commit-sha>
```

The clean, checked-out feature branch's exact submitted HEAD, the named base branch, and its full commit SHA are immutable run pins. The base must already be an ancestor of that HEAD and must still be the live base tip. Pins survive additive fixes and daemon recovery; `axi status` and drive output show them under `run.preserve_history`. A moved or unreadable base, lost submitted-head ancestry, or an unverified/non-fast-forward publication target stops with a `preserve-history` diagnostic. No rebase or merge is attempted, regardless of `rebase.strategy` or repair budgets. CI merge-conflict repair parks rather than launching a resolver. Additive repairs remain allowed under the existing revalidation policy, and every branch publication uses ordinary Git push, never force or force-with-lease; Git rejects a racing non-fast-forward update.

This is per-run, not repository configuration. It cannot be combined with `--skip` or `--existing-pr`, and because it requires `--base-branch` it cannot start on a branch with a remembered [existing PR association](#explicit-existing-upstream-pr) until that association is retired. A matching active run can be reattached to without changing its pins, and no other launch supersedes an active protected run: `axi run` refuses before pushing whenever the caller is not at that run's head, and a push that reaches the gate directly is refused with the command that restores the gate branch to the run's submitted head. With explicit receipt flags, replay requires the same pins and intent, and a new nonce refuses an already-active branch. `rerun` cannot implicitly repin a protected run: recover custody if necessary, explicitly prepare the desired integration, and start a new constrained run. Ordinary callers without the flag keep their existing behavior.

Both the CLI **and running daemon** must support this flag. Constrained launch uses a distinct RPC and imports the exact local commit without a gate push, so an older daemon or receive hook cannot silently start an unconstrained run. There is no legacy fallback or automatic daemon restart. These are pipeline-operation guards, not a sandbox against arbitrary trusted commands or agent tools; agents receive no extra history guidance, and ancestry is rechecked before publication. The mode does not create an attestation for prior work, skip validation, or merge a PR.

### Explicit existing upstream PR

When `origin` points at your fork but the PR lives upstream, select the existing review object explicitly:

```sh
no-mistakes axi run \
  --existing-pr https://github.com/upstream/widgets/pull/168 \
  --intent "Validate the account-context change in the existing upstream PR"
```

Run this from the PR's clean, checked-out source branch. The URL supplies both the upstream repository and PR number; no remote rewrite or separate repository flag is needed. This mode currently supports canonical `https://github.com/OWNER/REPO/pull/NUMBER` URLs only, not GitHub Enterprise Server or other providers.

Before any pipeline step starts, the daemon requires an open PR whose returned URL, number and base repository match the request, whose full source repository matches the configured push target (`fork_url` when configured, otherwise `origin`), and whose source branch and full head SHA match the submitted local branch and commit. **The source head must already be published**: local-only commits, stale local heads, deleted forks, closed/merged PRs, authentication failures and unavailable or incomplete validation fail rather than select or create another PR. A newer CLI also refuses an older daemon that lacks this launch protocol; it does not fall back to the ordinary push hook.

The association is stored atomically with the run and survives recovery and `rerun`. PR updates, pre-push attestation writes and CI target that exact upstream PR, without branch-based discovery. The PR's existing title and body are the author's: an explicit target is only ever appended to, through the same owned-appendix mechanism [`pr.template`](/no-mistakes/reference/repo-config/#prtemplate) uses, so the run adds its evidence section and never redrafts the description. An empty description stays empty - a repository template fills a blank body on that repository's own pull requests, never on an associated one. Before each pipeline push, the PR's live head must match the source remote head verified by the existing push-safety checks; publication and CI also validate the published head. Pipeline fixes still push to the source repository. Integration-branch fetches use the explicit upstream repository, while trusted configuration still comes from the registered repository's pinned default branch. The integration branch is the one the PR actually targets, read from the PR at launch and stored on the run, so Rebase, PR and CI all use it instead of the repository default or [`pr.base_branch`](/no-mistakes/reference/repo-config/#prbase_branch), and every gate measures its diff from that branch rather than your fork's default. It is fetched at launch, so an unreachable upstream refuses the run instead of silently reviewing against the fork. The gates then measure from that validated snapshot; only CI repair re-reads the branch, because a maintainer can retarget the PR mid-run. If the branch it must measure against cannot be read, the run stops: no other branch and no empty diff is ever substituted for it. This option does not retarget the PR.

Like every other launch, this one binds the branch in the daemon's gate repository to the submitted commit, which is what lets `rerun` and recovery read the validated head after a run that stopped at a gate. It only ever advances that branch: if the gate holds a head your submission does not contain, the launch is refused and you reconcile the branch (`no-mistakes sync`) first.

`--existing-pr` cannot be combined with `--base-branch`, `--skip`, or the strict launch-receipt flags. Reattach with the same URL or omit it; reattaching needs no `--intent` and accepts either the commit you submitted or the head the run has since moved to (for example after you fetch its fixes). A different or absent explicit target on an active run is refused with exit code `2` when you supply the flag. The same two restrictions hold for every later run on an associated branch, flag or not: a run that publishes to someone else's pull request delivers the whole pipeline, and its base comes from that pull request, so `--skip` (including a `no-mistakes.skip=` push option) and `--base-branch` are refused at launch until the association is retired.

#### Association lifecycle

A validated association belongs to the **branch**, so you name the pull request once:

| Step | Command | Effect |
| --- | --- | --- |
| Establish | `axi run --existing-pr <url> --intent "..."` | Validates the target and records it for this branch |
| Reuse | `axi run --intent "..."`, a `git push` that fires the hook, `rerun`, CI repair | Publishes to the recorded pull request; no discovery, no new PR |
| Replace | `axi run --existing-pr <other-url> --intent "..."` | Validates the new target and replaces the record |
| Retire | `axi run --retire-existing-pr` | Removes the record and starts no run; later runs use ordinary discovery. It refuses the flags that describe a run (`--intent`, `--skip`, `--base-branch`, `--preserve-history`, `--existing-pr`, `--launch-nonce`, `--validation-generation`) rather than discarding them; `--yes` and `--wait` are accepted and have nothing to do |

Establishing and replacing require the submitted head to already be the pull request's live head, because you are asserting which commit the target is at. A reused association does not: those runs exist to publish a new head, so they prove the association itself - the pull request is open, lives in the recorded repository, and its source is this push repository and branch - and the head is proven against the remote before the push and again by the PR and CI steps. A head this run just published is re-read a bounded number of times before a mismatch is reported, because the forge serves pull requests from a replica; the comparison itself stays exact, and an ancestor is never accepted. Either way a stale, ambiguous or unreadable target - a merged or closed pull request, a source ref that moved - fails the run rather than discovering, creating or updating a different pull request, and the failure names the branch, its pull request and the retire command. Nothing is retired automatically. Retiring is refused while a run is active on the branch, so an in-flight run never changes where it publishes.

Branches with no association are untouched: ordinary repository-scoped discovery is unchanged, and an `origin` pointing at a fork does **not** automatically discover upstream PRs. For general fork routing, including new PR creation, use [`init --fork-url`](#no-mistakes-init).

### Strict launch receipts

Supply `--launch-nonce` and `--validation-generation` together to bind a launch to an exact request instead of reattaching by branch and head alone.
Both identifiers must be 1–128 ASCII characters from `A-Z`, `a-z`, `0-9`, `.`, `_`, `~`, and `-`; they are non-secret correlation values and must not contain credentials.
This mode requires the same exact `--intent` bytes on retries.

```sh
no-mistakes axi run --intent "the user's goal" \
  --launch-nonce request-42 --validation-generation validation-3 \
  --base-branch epic/foo
```

After durable creation or claim, AXI writes a TOON `launch_receipt` object to stdout **before** driving the run.
The receipt remains available to a caller that captures stdout even if driving later stops at a gate or fails.
It contains `run_id`, `disposition` (`created` or `reused`), `launch_nonce`, `validation_generation`, `branch`, `head_sha`, `submitted_head_sha`, and `intent_digest`.
Both head fields contain the full, immutable submitted commit SHA; `intent_digest` is the lowercase SHA-256 digest of the exact persisted intent bytes, including whitespace.
Raw intent is not included in receipts, and generic run/status output does not expose the nonce binding or intent digest.

The nonce is scoped to the repository and branch.
The first successful receipt claim returns `created`; subsequent matching claims return `reused` for that same run, including after the gate or pipeline head advances.
A conflicting submitted head, validation generation, or intent is refused.
A different nonce creates a distinct run rather than reattaching to a same-head run; both post-receive creation and the up-to-date-push fallback follow this contract.
The up-to-date-push fallback preserves the latest same-head run's PR URL unless its recorded PR state is closed or merged, including when an explicit `--base-branch` retargets that PR, subject to [run-scoped Test preview ownership](#no-mistakes-axi-publish-test-preview).
An explicit `--base-branch` is persisted on creation and must match the stored per-run base on replay; omitting it on replay preserves the stored base.
A conflicting claim does not consume the first `created` disposition.
Without the two proof flags, ordinary reattachment is unchanged, and historical runs without a nonce are not adopted into a proof binding.

## no-mistakes axi publish-test-preview

Publish the exact committed head of an active run before Test executes, or while Test is parked, without waiving validation. This operation is never invoked automatically:

```sh
no-mistakes axi status --run <run-id>
no-mistakes axi publish-test-preview --run <run-id> --head <full-current-head-sha>
```

Both flags are required. While Test is pending, invoke this command during an earlier step: the request waits for the serialized pre-Test boundary, publishes before Test's first attempt, and leaves Test scheduled to execute afterwards. Requests expire after two minutes; an expired request is not published later. The exact head is checked again at that boundary; changes made by an earlier step require fresh operator evidence and another invocation. The command waits up to three minutes for the daemon's response. At a settled Test gate, publication is serialized against gate responses and preserves the gate and findings. Running/fixing Test, terminal runs, skipped Test, protected-path, interrupted-repair and shutdown gates are refused, as are dirty worktrees and unfinished integrations. The command does not commit files, run formatters, launch agents, spend repair rounds, approve Test, or complete Push/PR/CI. It uses the ordinary guarded publication path, including review-head continuity, history constraints, remote verification and durable custody binding. Branch custody stays with the run.

Currently this operation supports run-owned github.com PRs only, not `--existing-pr` associations. It creates a **draft** PR labeled **`pr-validation`** and explicitly describes it as a PR validation preview. Label provisioning and application are mandatory: the daemon creates the repository label if absent without overwriting an existing label, and reports label errors even if the draft has already been published. The credentials therefore need PR and label write access. An existing PR is reusable only when bound to this run's preview (or recoverable from its run-specific creation marker); it must still be draft with auto-merge disabled and target the same base. Repeating the command reuses the same PR, including after a lost response. Missing, foreign, closed or already-ready PRs fail closed instead of opening a replacement. Existing author prose is preserved; pre-push attestation updates remain confined to pipeline-owned evidence.

Preview provisioning remains repository-owned: configure preview automation to accept draft PRs and the `pr-validation` label, with an environment name containing `preview`. The command reports `pr_url`, the exact `head_sha`, `label`, the unchanged `test_status`, and `preview_url`. A URL is reported only for a successful GitHub deployment of **that exact SHA**, never a stale branch preview or production deployment. An empty URL means pending or not configured, **not successful live validation**; repeat the same command to discover a deployment later. This operation does not create a hosting account, change provider budgets, or supply a local container fallback.

The PR attestation records Test's actual pending or parked status, which does not satisfy `require-no-mistakes`. No scenario becomes passed, skipped or not applicable. Until a post-publication Test execution succeeds, approving its gate reruns Test rather than waiving validation; this rule survives daemon recovery. A skipped, failed or waived attempt cannot ready the preview. All remaining gates still execute in their original order. Only the later normal PR step, after successful post-publication Test execution and ordinary publication, replaces the owned preview narrative and makes this run's draft ready (unless provider configuration deliberately keeps PRs draft).

That guard requires successful post-publication Test execution, not equality between its tested head and every later run head. Authorized Document/Lint or formatter commits do not strand the draft, and later CI repair revalidation uses the ordinary Test flow without requiring a preview of the unpublished repair. [Head-bound evidence rendering](/no-mistakes/reference/pipeline-steps/#pipeline-step-attestation) prevents those later heads from claiming an earlier head's live-validation verdict.

Preview ownership stays strictly run-scoped: the shared branch-replacement boundary refuses a new run while any run on that branch owns an open managed preview. This includes ordinary and proof-mode pushes (with or without a previous head), fresh launches, reruns, history-preserving launches and explicit existing-PR launches; it does not depend on matching the old head or inheriting its PR URL. Replaying the same launch receipt still reuses its original run. No replacement transfers preview ownership.

Replacement and inheritance checks read each bound preview's live forge lifecycle rather than relying on its cached PR state. A confirmed closed or merged preview no longer blocks replacement and is not inherited; an unreadable or unknown lifecycle refuses the launch. If draft creation succeeded before its binding was recorded, the check scans all matching open PRs across base branches for run-specific creation markers before concluding there is no owner. Such a marker preserves the original run's ownership without creating or transferring a binding. The draft prevents early auto-merge eligibility; manual external changes to draft/auto-merge state are refused on preview retries.

Both CLI and daemon must support this distinct RPC. An older daemon rejects it; there is no publication fallback or automatic restart/update. Installing a new binary does not change the behavior of a daemon already running. Activate a new daemon version only through an explicitly authorized lifecycle window, never by restarting a shared daemon to make this command work.

## no-mistakes axi republish-publication

Continue the PR publication of an **already-terminal** run whose PR step failed, without re-running any validation step:

```sh
no-mistakes axi status --run <run-id>
no-mistakes axi republish-publication --run <run-id> --head <recorded-full-head-sha>
```

Both flags are required; there is no latest-run fallback. The run must be terminal (`failed`, `cancelled`, `completed`, or `ci_monitor_interrupted`), its PR step must have failed, and its recorded head must equal `--head` exactly. It must also carry its own recorded PR target branch (`pr_base_branch`); current configuration cannot substitute for a missing pin or redirect publication.

The daemon waits for any remaining cleanup of the original run, verifies the live remote source branch still equals the recorded head, and creates a throwaway detached worktree at that head. It executes the ordinary PR step against the recorded step rows and pinned target: publication validation, identifier redaction, template/ownership handling and bounded sanitize-and-retry still apply. A skipped PR operation (for example, because forge authentication is unavailable), a missing outcome, or an empty PR URL is an error, not successful republication. A whole-pipeline `rerun` remains the path when the validation evidence itself must be produced again.

Recorded step statuses are rendered exactly as recorded. No validation step is re-run, skipped, invented or waived; a missing or non-passing step stays missing or non-passing in the published evidence, and `require-no-mistakes` still refuses it. Nothing is pushed (the run's head is already on the remote). Fresh evidence publication and media attachment are disabled, so only already-persisted evidence is reused. Run status, custody and shared gate refs are unchanged; config and policy reads use operation-private refs that are deleted afterwards. Apart from the PR publication and its local log, the only persisted update is the run's PR URL when publication succeeds and none was recorded.

Refused, with no fallback, for: a non-terminal run; an active run on the branch (its executor owns publication); a head mismatch with the run's record; a remote source branch that no longer equals the recorded head (a newer publication owns the branch - recover custody with `no-mistakes axi sync --check` instead); a run with no recorded PR target branch; a run whose PR step did not fail (nothing to continue); a preserve-history run; or a recorded head absent from the gate repository (recover custody with `no-mistakes axi sync --check` first). This operation never establishes CI readiness, merge authority, or a green validation claim; ordinary CI monitoring and review still apply to the PR.

Both CLI and daemon must support this distinct RPC. An older daemon rejects it; there is no publication fallback, automatic restart/update, or rerun substitution.

## no-mistakes axi refresh-publication

Append a separately attributed current-head repair disposition while the same run is still monitoring CI, without re-running validation or answering an approval gate:

```sh
no-mistakes axi refresh-publication --run <run-id> --pr <recorded-pr-url> --head <full-pushed-head-sha>
```

All three flags are required. The daemon dispatches the operation through that run's CI monitor between polls, never concurrently with its repairs. The exact repository, run, recorded PR URL, full recorded head, last-pushed head, remote source head and numbered live PR/source identity must agree before publication and again just before the write. A recorded target-branch pin must still agree, and the live target must not change during preparation. It requires raw PR content and exact source-identity support (currently canonical `https://github.com/owner/repo/pull/number` URLs only); unsupported adapters refuse. A single existing pipeline attestation must already name that same head; refresh never inserts or restamps one. Fixing, parked, terminal, mismatched and unreadable identity states refuse without a publication fallback.

The refresh preserves historical PR content and the existing attestation byte-for-byte, including review risk and findings, step statuses, skips and timeouts. It adds or replaces only its own integrity-checked disposition block; an edited block or concurrently changed PR body refuses rather than discarding notes. Owned appendices retain their ownership guard and author text. No source/evidence push, media upload, agent invocation, config/policy fetch, validation replay, history edit, label change or merge authorization occurs. The only external mutation is a body-only update of the recorded PR.

The disposition reports a timestamped live-forge check snapshot and recorded published CI repair rounds. Missing or unreadable checks are inconclusive; failures, pending/unknown checks and skips are not passing tests. Even all passing checks do **not** prove a historical finding fixed, lower review risk, or establish live product behavior. Finding-specific correctness remains unproven by this operation; historical review metadata is not rewritten into a new verdict. It does not set CI readiness. As with other provider body updates, the final identity/body recheck is not a forge-side atomic compare-and-swap.

This is distinct from terminal failed-publication continuation above. Both CLI and the running daemon must support the new RPC; an older daemon refuses without automatic restart/update. The call is bounded to three minutes, including monitor dispatch; inspect status before retrying an interrupted call because a provider may have accepted its write.

## no-mistakes axi respond

Answer the current approval gate and continue until the next gate, CI-ready decision point, or final outcome.

```sh
no-mistakes axi respond --action approve
no-mistakes axi respond --action fix --finding F1 --finding F2 --instructions "optional guidance"
no-mistakes axi respond --action fix --add-finding '{"description":"...","action":"auto-fix"}'
no-mistakes axi respond --action skip
no-mistakes axi respond --action abort # only repair-reconciliation or daemon-shutdown gates
```

| Flag             | Type     | Default       | Description                                                          |
| ---------------- | -------- | ------------- | -------------------------------------------------------------------- |
| `--action`       | `string` | (none)        | `approve`, `fix`, `skip`, or gate-specific `abort`; required         |
| `--step`         | `string` | awaiting step | Step to respond to                                                   |
| `--finding`      | `string[]` | (none)      | Exact finding ID for `--action fix`; repeat for multiple IDs         |
| `--findings`     | `string` | (none)        | Legacy comma-separated finding IDs; use `--finding` for arbitrary IDs |
| `--instructions` | `string` | (none)        | Guidance applied to selected findings                                |
| `--add-finding`  | `string` | (none)        | JSON finding object to add and fix                                   |
| `-y`, `--yes`    | `bool`   | `false`       | Auto-resolve subsequent eligible gates until a decision point or outcome |
| `--wait`         | `duration` | `8m`        | Maximum time for pre-drive reads and post-response driving before the caller must reattach |

A repair-budget gate follows the [one-repair explicit response rule](/no-mistakes/concepts/auto-fix/#how-it-works). A repair-reconciliation gate accepts `fix` only when no repair process identity was registered; otherwise use `abort` rather than replaying an uncertain repair. A `daemon-shutdown` gate accepts only `abort` because the interrupted ordinary step cannot be resumed or trusted. `--action abort` is rejected at every other gate; use [`axi abort`](#no-mistakes-axi-abort) to cancel an ordinary active run.

At a CI gate, `axi respond --action approve --yes` and `axi respond --action skip --yes` are rejected with exit code `2` before sending a response, including when `--step babysit` uses the legacy CI alias. Omit `--yes` only for an explicit operator decision to approve or skip; `--action fix --yes` remains available.
After an accepted response, `--yes` uses the same [auto-resolution behavior and exceptions as `axi run --yes`](#no-mistakes-axi-run).
Each `axi respond` blocks until the next gate, CI-ready decision point, or final outcome, subject to the same default `--wait 8m` boundary as `axi run`. That boundary also covers its initial active-run and run-state reads plus event-subscription acknowledgement, so a caller can interrupt establishment as well as the later event wait.
If it returns another `gate:`, answer that gate; do not idle-wait for the run to move forward by itself.
When the daemon is already running, `axi respond` can continue an active run even if the global config file has become invalid, because it is not starting a fresh run.
The same successful-output reporting instructions apply to `axi respond` results.

## no-mistakes axi status

When `--run` is omitted, show this branch's run: its active run, else its most recent one.
Resolution is scoped to the current branch and never falls back to another branch's run, because one clone commonly has several worktrees on different branches.
On a successful status response, when the current branch has no run of its own - including a detached `HEAD`, which owns no branch and so reports `current_branch: unknown` - the output carries no run object at all.
It reports `current_branch`, `runs_on_current_branch: 0` where a branch is known, and the recent-runs listing, so an unrelated run can never be read as this worktree's.
If the implicit current-branch lookup itself fails, status returns that error instead of presenting the failure as a detached or no-run result.
Detached-`HEAD` help offers deliberate `--run <id>` inspection or checking out a branch; it does not offer `axi run`, which requires a branch.
With `--run <id>`, inspect exactly that run regardless of branch; when its branch differs from a known current branch, it is rendered under `other_branch_run:` instead of `run:`, alongside a top-level `current_branch`, so a parser keyed on `run:` never picks up a run proven to be on another branch.
An explicit `--run <id>` rendered under `run:` while the current branch is unknown (detached `HEAD` or a branch-lookup failure) encodes no branch relationship.

```sh
no-mistakes axi status
no-mistakes axi status --run <id>
```

| Flag    | Type     | Default            | Description               |
| ------- | -------- | ------------------ | ------------------------- |
| `--run` | `string` | current-branch run | Inspect a specific run ID |

When the resolved run is parked at an `awaiting_approval` or `fix_review` gate, its top-level `run:` or `other_branch_run:` object includes `awaiting_agent: parked <duration>` immediately after `status`.
The field disappears after that run's gate is answered, on cancel, and on terminal outcomes; use it to distinguish a run waiting for the driving agent from one actively running, fixing, or watching CI.
When an interrupted repair has retained local edits, the run object includes `survived_work` (its `state` is `captured`, `restored`, or `in_place`). The [CI step reference](/no-mistakes/reference/pipeline-steps/#ci) owns the retention, handoff, and publication-safety semantics.
Status offers branch-scoped `axi respond` commands only for the current branch's implicitly resolved run; `axi respond` has no run selector and answers the branch's current active run. An explicitly selected gate stays inspection-only: its log commands retain `--run <id>`, and the one exception - a stale cancelled-checks CI `fix_review` gate - is guidance to verify, not a command. That suggestion appears only when the invoking worktree is on the run's own branch and its `HEAD` equals the run's immutable submitted head (not its current pipeline head), and it directs the operator to confirm the branch's active run is still the inspected one before answering from the branch's own worktree. A foreign branch, a missing submitted-head pin, a mismatched or unreadable head, or any other explicit selection earns no response guidance.
When a repository has no configured lint command and Document performs the combined Document/Lint housekeeping invocation, the run object includes `shared_work` evidence naming its `document+lint housekeeping` scope and the duration attributed to Document; Lint's own duration remains the cached-result handoff time.
When the resolved run has a `running` or `fixing` step, the run object includes `active_steps`.
Each row reports the whole step's elapsed time as `active_for`, the displayed execution or fix round's elapsed time as `round_active_for`, the latest meaningful log or native-agent lifecycle activity, the native agent PID if one is currently running, and the current round such as `round 1`, `auto-fix 1/3`, or `fix 2`.
`round_active_for` resets when a fix round starts; older active runs created before this timing was recorded show it as empty.
If no activity arrives for longer than `step_quiet_warning`, `last_activity` is prefixed with `quiet`; this is only a liveness signal and does not cancel the step.
For older active runs with no recorded activity timestamp, AXI falls back to the step log file modification time.
Gate summaries and finding descriptions are bounded in this default status view; truncated values disclose their original length, and the gate help points to `no-mistakes axi logs --step <step> --full` for an implicitly resolved run or `no-mistakes axi logs --run <id> --step <step> --full` for an explicitly selected run.
Relevant current-branch states also include a cached `branch_sync` object with full SHAs, the run's status, the persisted pipeline push binding, target kind and ref, relation, safety result, PR lifecycle, and a structured next action.
Cached home and status rendering performs no network read and labels the remote observation `pipeline_push`; only explicit sync check or apply reports `live` freshness.

## no-mistakes axi sync

Freshly check or apply the guarded synchronization offered by a `branch_sync.next_action`.

```sh
no-mistakes axi sync --check
no-mistakes axi sync
no-mistakes axi sync --recover
no-mistakes axi sync --recover --keep-local
no-mistakes axi sync --release
no-mistakes axi sync --bind-archive-ref refs/heads/archive/<name>
```

| Flag                 | Type     | Default | Description                                                                  |
| -------------------- | -------- | ------- | ---------------------------------------------------------------------------- |
| `--check`            | `bool`   | `false` | Verify the live target and exact plan without changing `HEAD`                |
| `--recover`          | `bool`   | `false` | Return custody of a branch stranded by a terminal run with unpublished pipeline commits (a no-op when cancellation already released the branch) |
| `--keep-local`       | `bool`   | `false` | With `--recover`: keep the current local head; never touches the worktree   |
| `--release`          | `bool`   | `false` | Release a terminal run's stale push binding whose live remote no longer matches it (`next_action.code: release_binding`); mutually exclusive with recovery and archive binding |
| `--bind-archive-ref` | `string` | (none)  | Bind one existing `refs/heads/archive/*` commit as exact evidence for a keep-local recovery; never creates or moves a Git ref |

The default command is an explicit non-interactive apply request and never prompts.
All modes return the complete `branch_sync` object as TOON.
Exit code `0` means an eligible check, applied synchronization, recovery, or binding release, already-synchronized, custody-returned, or user-owned no-op, or expected merged-and-removed no-op; blocked operational states return `1`.
The ordinary worktree mutation is either a strict fast-forward of the invoking clean checked-out branch to the freshly verified pipeline-owned pushed SHA, or an equivalent-diverged advance.
When a clean local branch and the pipeline-pushed head are diverged but the local unique work is content-equivalent to work already represented in the live pipeline head, `sync` reports `safety: safe_equivalent_advance`, anchors the pre-sync head under `refs/no-mistakes/sync-anchor/<run>`, and moves to the pipeline head with reset semantics.
Genuine divergence still reports `safety: blocked_diverged` and changes nothing during ordinary synchronization.
A normally closed, unmerged PR does not strand its published pipeline changes: after the run is terminal, if its final head equals the exact pushed binding and differs from its submitted head, ordinary `axi sync --check` / `axi sync` can verify and fast-forward the clean caller branch to those commits before any new validation run. The live remote must still equal that binding, and the recorded PR state must remain closed through the pre-apply checks. Neither `--recover` nor `--release` applies to this intact published binding. This closed-PR path never uses an equivalent-diverged reset; divergence stays blocked, and merged PR retirement is unchanged. Synchronization neither reopens the PR nor changes recorded run state or stamps a custody-return override.
Under `--recover`, the possible worktree mutation is a strict fast-forward to the preserved pipeline head, or an adoption of a preserved head proven to carry every local change, both after relation-specific preservation checks. The bound-archive exception described below never changes the worktree at all.
When the local gate branch is exactly at a newer same-branch pushed binding and Git proves that it retains an older terminal run's unpublished changes—either by containing the recorded commits or by an unambiguous ordered replay of their patch identities—branch synchronization selects the newer binding. Missing gate evidence, unretained or ambiguous changes, and different or ambiguous target provenance remain blocked.

A separate, read-only selection exception skips an older terminal explicit-PR preflight failure proven never to have started execution: its recorded launch-validation error must identify that route, its head must equal its nonempty submitted head, and it must have no step or agent-invocation rows, pinned gates, publication binding (even partial), live push, or custody-return stamp. A recorded worktree directory must be absent; an unreadable directory does not qualify. Absence of execution rows alone is not proof. A strictly newer terminal run in the same repository and exact branch must have submitted that same head and carry a verified terminal head with an exact current push binding, no live push, and an exact non-symbolic gate branch matching the binding. All eligible newer bindings are considered, so a later rerun does not revive the obsolete obstruction. Active runs retain ownership. This exception applies to inspection and recovery-stack eligibility; it never rewrites history, stamps verification, or changes publication bindings. The selected publication still undergoes ordinary live-remote, clean-worktree, and containment checks: matching submitted heads does not certify content retention, and genuine divergence remains blocked.

Fork configurations verify the configured fork URL and exact feature ref rather than assuming `origin`.
Dirty, in-progress, ahead, genuinely diverged, detached, wrong-branch, offline, changed-target, rewritten, deleted, legacy, or retired states fail closed without destructive recovery.
Run `axi sync` only when structured output offers `next_action.code: sync`; process any blocked state instead of substituting reset, stash, merge, rebase, force, or branch replacement.

### Stale-binding release

When someone rewrites or deletes the remote branch after a terminal run pushed it, the persisted push binding can never be satisfied again and the branch is stranded: status reports `safety: blocked_remote_rewritten` (or `blocked_remote_advanced` / `blocked_remote_missing`), always with `next_action.code: release_binding`. `--release` is the sanctioned exit: it requires a terminal, non-active run whose exact recorded binding matches the branch and target, a clean invoking worktree, and a live remote that no longer equals the binding; it anchors the run's recorded pipeline head - under the exact binding, the previously pushed head - at the run recovery ref whenever the local branch does not already keep it reachable, anchors any independently moved gate head at the gate recovery ref, re-points the local gate branch to the current local head with a compare-and-swap, and stamps custody. It never force-pushes or otherwise touches the changed remote - reconciling that history stays the operator's choice. Release is idempotent (a second run is a no-op) and mutually exclusive with recovery and archive binding. A run that went terminal with unpublished pipeline work after its push is served by ordinary `recover_custody` instead; release applies to the exact-binding shape only.

### Custody recovery

A run that goes terminal (cancelled, failed, or completed without a push stage) after moving the pipeline head leaves the branch `pipeline_owned`. Status offers `next_action.code: recover_custody` only when recovery can establish the same eligibility it will enforce: an equal or ahead local head proves the source locally and can create the local anchor when the gate is unavailable, but any existing gate recovery ref must still match the recorded head; importing a missing preserved head requires an exact run-specific gate anchor (or legacy commit evidence that can be anchored), a clean worktree, and either local ancestry or the content-preservation proof described below. The archive-backed keep-local exception has its own stricter proof below. An eligible state reports `safety: blocked_pipeline_owned_recoverable`, the run's terminal `pipeline.status`, and the exact `submitted_head`/`current_head`/`relation` ownership facts.
A run whose terminalization verifies that the managed worktree head never changed from the submitted head releases the branch instead: the terminal outcome, including cancellation, ends ownership; status reports `state: user_owned` with the same exact ownership facts and no `next_action`; the branch and head are immediately usable for any separately authorized delivery; and nothing blocks a direct push or PR.
Without positive evidence that the submitted head stayed unchanged, custody is not guessed away. Conflicting evidence, import cases with a dirty worktree, and genuinely divergent history retain manual-reconciliation guidance instead of being labeled as a missing preserved head, unless the daemon's private mirror still holds the verified head as described below. When a verified recorded head is absent from both the invoking worktree and an accessible local gate, with compatible recovery refs, status reports `safety: blocked_recover_preserved_head_missing` with `next_action.code: recover_custody` and `no-mistakes axi sync --recover --keep-local`: that flag is the operator's explicit choice to keep the current local head and discard the missing preserved commits. When such a verified recorded head is intact in the daemon's private gate mirror while the invoking worktree has never received it - including a dirty worktree, which can never adopt it - status keeps `safety: blocked_pipeline_owned_recoverable` but attaches recovery evidence with `source: gate_mirror` and `keep_local: true`, offering the same `--recover --keep-local` command: those commits are not unrecoverable, custody returns at the current local head, and the preserved head stays anchored. A worktree that already holds the preserved object keeps manual reconciliation, because it can inspect both divergent heads itself. An unavailable gate remains blocked because absence cannot be established there. The archive-backed keep-local exception has its own stricter proof below.
While a run is still active, it reports `state: pipeline_owned`, the exact submitted/current heads and their relation, and `next_action.code: continue_active_run` with `no-mistakes axi status`, even when its head has not moved yet.
`--recover` verifies the run is terminal, anchors the preserved head under `refs/no-mistakes/recover/<run>` in the invoking repository, and stamps custody returned so a fresh run can start.
For equal or ahead worktrees where the preserved head is already locally reachable, recovery writes that anchor locally without requiring gate access. If the gate is available, an existing symbolic, non-commit, or mismatched recovery ref is conflicting evidence and recovery refuses without overwriting it.
For behind or diverged worktrees, recovery verifies the preserved head at the run-specific recovery ref in the local gate and fetches it into the anchor before moving or refusing. Legacy recorded heads that remain available as unreferenced gate objects are anchored before recovery continues.
A clean behind worktree fast-forwards.
A diverged worktree is adopted only when the preserved head provably carries every local change, proven by an executable three-way merge whose result is exactly the preserved head's tree.
This covers a pipeline rebase onto a newer base without requiring the gate branch to advance to the preserved head.
Terminalization pins a verified unpublished pipeline head under a run-specific recovery ref, so recovery does not require the gate branch itself to have advanced. If the verified recorded head is absent from both the worktree and an accessible gate and the recovery refs are compatible, status still offers `recover_custody`, but the command is `--recover --keep-local` rather than taking that head. Plain `--recover` without `--keep-local` still refuses.
That adoption anchors the pre-recovery local head under `refs/no-mistakes/recover-local/<run>`, then moves the branch with Git operations that refuse on their own rather than after a preceding check: an atomic compare-and-swap on the branch ref, and a working-tree update that aborts instead of overwriting a modified or untracked file.
The proof is deliberately narrow and never uses patch identity, which discards hunk locations and whitespace and so cannot tell a genuine replay from a same-shaped edit elsewhere.
Anything it cannot decide - unlanded local commits, or a rebase whose fix rounds also rewrote your own lines - still refuses with the anchor named, because only escalation can tell a deliberate pipeline fix apart from a dropped change.
A dirty worktree refuses with explicit choices.
When you explicitly keep a behind or diverged local head instead of taking the preserved head, `--keep-local` returns custody at the current head without touching the worktree and atomically points the gate branch at it. `--keep-local` is also the recovery when an accessible gate confirms that the verified recorded head is missing and the recovery refs are compatible: the operator is keeping the current local head and discarding those unpublished commits, so that missing object does not block the path. One `--keep-local` call releases the full stranded stack: genuinely missing heads are explicitly discarded, while still-available heads are anchored under their run-specific recovery refs first. Unverified or conflicting evidence on any run blocks the release before any custody stamp. If the gate branch moved independently, recovery first preserves that head under `refs/no-mistakes/recover-gate/<run>`; a conflicting pre-existing anchor makes recovery refuse, and a concurrent gate push wins the compare-and-swap and also makes recovery refuse.

A narrower archive-backed path handles a divergent later validation head that must be preserved but must not become the working result. First create the archive ref through a trusted preservation workflow, then explicitly bind that already-existing ref with `--bind-archive-ref`. Binding writes only an append-only recovery-archive record; it never creates, moves, fetches, or deletes a Git ref. The record snapshots the repository ID, lookup and evidence run IDs, branch, exact required working head, exact preserved later head, archive ref, and creation time.

Status uses the same `recoverySourceAvailable` classification as recovery and accepts this path only when exactly one record matches the selected terminal run, the clean checked-out branch remains at an exact submitted, reviewed, or successfully pushed required head, the later recorded head genuinely diverges, the raw archive ref is a non-symbolic commit at that exact later head, and the gate's run-specific recovery ref independently still pins it. The gate branch itself must be at either the required head or the preserved head. It revalidates the repository, run, branch, both full SHAs, archive ref, gate branch, and every existing recovery anchor before each action. Missing, moved, replaced, symbolic, malformed, cross-repository, wrong-run, wrong-branch, stale, hash-mismatched, or multiple records fail closed with `next_action.code: inspect_and_reconcile_manually`; similarly named branches, reflogs, loose objects, remote-tracking refs, and tags are never scanned or inferred as proof.

A verified archive plan reports its evidence in `branch_sync.recovery`, keeps `next_action.code: recover_custody`, and sets the command to exactly `no-mistakes axi sync --recover --keep-local`. That action leaves the worktree and local branch at `recovery.required_head`, leaves the divergent archive at `recovery.preserved_head`, and never merges, replays, fast-forwards, resets to, or otherwise selects the archived history. It also pins both exact heads locally at `refs/no-mistakes/recover-local/<run>` (required) and `refs/no-mistakes/recover/<run>` (preserved), without replacing any conflicting anchor. Running plain `--recover` against this plan refuses without adding an anchor or moving a ref.

The same bound-archive path supports one published terminal-divergence shape: a **failed**, terminally verified run whose clean caller remains at its exact submitted head, diverging from the run's exact final, successfully published head. The recorded branch and push-target binding must still match; the existing archive and run-specific gate recovery ref must both retain that published head. Bind the already-preserved archive, then follow the verified status command:

```sh
no-mistakes axi sync --bind-archive-ref refs/heads/archive/<existing-preservation-anchor>
no-mistakes axi status
# Only when branch_sync.next_action offers this exact command:
no-mistakes axi sync --recover --keep-local
```

This is custody return, not content adoption: both histories and all pipeline review/documentation fixes stay reachable, but neither is selected into the other. The caller's branch and clean worktree remain unchanged, the private gate branch is compare-and-swapped to the submitted head, and the external published branch is untouched. No merge, replay, reset, force-push, run restart, PR modification, or validation waiver is part of this action. A successful return reports `custody_returned`; any later validation or delivery is a separate action. Active or unknown runs, successful published runs, an unauthorized target, non-submitted caller heads, dirty worktrees, missing/moved/symbolic refs, and multiple archive records retain their existing refusal guidance. Without an explicit bound archive, this published divergence still requires manual reconciliation; ordinary synchronization is unchanged.

For the alternative validation path and its refusal conditions, see [`no-mistakes rerun`](#no-mistakes-rerun); it is not offered for the archive-backed keep-local plan.
A recovered never-pushed run reports `state: custody_returned`; a recovered pushed run ordinarily reports its classification against the last push binding, typically `local_ahead`. The failed published-divergence archive exception above instead reports `custody_returned` at the exact kept submitted head.
On a `user_owned` branch, `--recover` is an idempotent no-op success: nothing pipeline-created exists to recover, and no file, ref, or database row changes.

## no-mistakes axi logs

Show the log output of one pipeline step.

```sh
no-mistakes axi logs --step review
no-mistakes axi logs --step review --full
no-mistakes axi logs --step review --run <id>
no-mistakes axi logs --step gate.test.mutation-budget
```

| Flag     | Type     | Default            | Description                             |
| -------- | -------- | ------------------ | --------------------------------------- |
| `--step` | `string` | (none)             | Step name; required                     |
| `--run`  | `string` | current-branch run | Run ID to inspect                       |
| `--full` | `bool`   | `false`            | Show the entire log instead of the tail |

When `--run` is omitted, the run is resolved the same way as [`axi status`](#no-mistakes-axi-status): this branch's run, never another branch's.
With `--run <id>`, logs are read from exactly that run regardless of branch.
An unknown explicit run ID exits nonzero with `error: run "<id>" not found` instead of reporting that the current branch has no run.
Without `--full`, long logs show the last 40 lines and a help hint for the full log; when `--run <id>` selected the log, that hint retains the same run ID.
Step logs include native subprocess agent lifecycle lines such as `codex started pid=4242`, `codex exited pid=4242 status=success`, and transient retry messages when the selected agent supports lifecycle events.
They also include fix-loop markers such as `auto-fix round 1/3 starting after round 1` and `user-fix round starting after round 2`.
`--step` accepts the ten core step names and valid repository gate names such as `gate.test.mutation-budget`. Use the exact gate name shown by `axi status`.

## no-mistakes axi monitor-stop

Freeze monitoring for an explicitly selected publication without aborting validation,
responding to a gate, cancelling a repair, changing Git or the forge, or returning
edit custody. This is a one-way frozen disposition, not a successful validation or a
pause/resume interface. It never approves unfinished work.

```sh
no-mistakes axi monitor-stop --run <id> --repo-id <registered-repo-id> \
  --pr-url <exact-recorded-url> --published-head <full-last-pushed-sha> \
  --observed-head <full-current-pipeline-sha> --state parked-review
```

Read `axi status --run <id>` for the recorded `repo_id`, `pr` (`--pr-url`),
`last_pushed_sha` (`--published-head`), and full `head_sha` (`--observed-head`).
All identity flags are required and rechecked against durable records; the run
must be running with a recorded open PR and no active push.
`--state parked-review` requires an unfinished Review approval gate with CI
still pending; it preserves the gate and all findings. It does **not** assert that a
historical monitor caused that Review: legacy runs do not record that origin.
`--state polling` requires a running, post-green CI monitor at the exact published
head with all prior steps completed or skipped. This includes green polling after
a published CI repair when the latest repair invocation durably finished and
entered post-repair monitoring; a still-open repair-budget dispatch alone does
not make that repair active. A polling request is acknowledged only at a quiescent
poll boundary, never by cancelling ongoing work. Active or uncertain repairs,
changed identity/state, incomplete evidence, absent owners, and an owner already
cancelled before the stop is accepted refuse the request. These two exact-pin
states are the only supported cessation boundaries; this is not a general
run-lifecycle control or readiness certification.
Invoke it outside managed validation worktrees, which refuse pipeline control.
The daemon checks the authenticated caller's OS-reported working directory as
well as its ancestry; unverifiable caller topology refuses the request.

Success reports `monitor_stopped: true`, `validation_finished: false`, and
`custody_returned: false`. The stored validation, readiness evidence, rounds,
publication binding and commits are unchanged. Live status exposes
`monitor_stopped: true` and no longer advertises old readiness as current green.
The frozen run keeps pipeline custody and accepts no gate response, cancellation,
or superseding launch. There is no same-run fix release. Reattaching with
`axi run` returns immediately with `outcome: monitor-stopped`, not validation
success or a custody transfer. [Daemon crash recovery](/no-mistakes/concepts/daemon/#crash-recovery)
preserves this disposition. Existing PR body, labels and draft status are untouched;
a do-not-merge publication must already have that policy set by its owner.

Repeating the exact request succeeds while its identity and observed state still
match. A timeout or disconnected client reports cessation **unconfirmed**, never
success; repeat the same exact request to reconcile a possibly accepted write.
The command uses the distinct `stop_monitor` RPC and never falls back to abort or
starts a daemon. Both CLI and the running daemon must contain this feature; an
older daemon refuses it. Installing only a newer CLI cannot add the owner boundary
to an already-running daemon. Coordinate a normal authorized daemon rollout before
application; do not restart a shared daemon to make this command available.

## no-mistakes axi abort

Cancel the active run for the current branch, unless it has a
[frozen monitor disposition](#no-mistakes-axi-monitor-stop), which refuses cancellation.
Active runs on other branches are left alone.

```sh
no-mistakes axi abort
```

If there is no active run, this succeeds as a no-op.

Pass `--run <id>` to cancel a specific run by its id instead of resolving the current branch:

```sh
no-mistakes axi abort --run <id>
```

`--run` does not need a repo, branch, or worktree, so it works from anywhere.
Use it to reap an orphaned CI monitor whose worktree was torn down before the PR merged - the run id is shown in `axi run` output and in the `axi` home view.
A `--run` id that is not currently active is resolved against the exact run's durable record rather than trusted blindly: a known already-terminal run returns an idempotent success carrying its terminal `run_status` with no fabricated new cancellation, a positively proven unknown id keeps the documented successful no-op with no fabricated state, and a run that is recorded as still nonterminal or cannot be read returns the nonzero terminal-unconfirmed contract.
When the daemon is not running, nothing can be cancelled and abort never starts one: the durable record alone decides the same three outcomes, and a recorded nonterminal run reports that cancellation could not be requested.
When the daemon is already running, `axi abort` can cancel an active run even if the global config file has become invalid, because it is not starting a fresh run.
Both abort surfaces report a completed cancellation only after the exact run positively confirms a terminal state within the bounded wait; success then includes the terminal `run_status`, and branch-scoped abort renders the refreshed `branch_sync` object and its exact next action, if any.
When terminal quiescence cannot be confirmed - the bounded wait expires, the wait is cancelled, or a status read fails - abort exits nonzero, states explicitly that cancellation was requested but terminal quiescence is unconfirmed, includes the last structured run state when one is available, and never claims `aborted: true` or presents user-owned or recoverable ownership guidance as authoritative; re-run the abort or watch `axi status --run <id>` until a terminal status is confirmed.
Pipeline-created commits remain preserved in the gate and a recoverable cancellation points directly to `no-mistakes axi sync --recover`; when the submitted head never moved, cancellation instead reports `state: user_owned` with no sync action.
While a run is active, do not use `axi abort` or `no-mistakes rerun` to go fix a finding yourself.
That cancels the pipeline's in-flight work and forces a full re-validation; use `axi respond --action fix` at the gate so the pipeline applies and re-checks the fix.

## no-mistakes eject

Remove the gate from the current repository.

```sh
no-mistakes eject
```

Removes the `no-mistakes` remote, deletes the bare repo directory, cleans up worktrees, and deletes the database record (cascades to runs and steps).
It does not remove any legacy repo-local agent skill files left by older versions; current `init` installs the skill at user level instead.

## no-mistakes attach

Attach to the active pipeline run.

```sh
no-mistakes attach [--run <id>]
```

| Flag    | Type     | Default | Description                                           |
| ------- | -------- | ------- | ----------------------------------------------------- |
| `--run` | `string` | (none)  | Attach to a specific run ID instead of the active run |

Opens the TUI for the active run anywhere in the current repo. If `--run` is specified, attaches to that specific run regardless of branch. Unlike bare `no-mistakes`, this does not stay branch-scoped before falling back.

## no-mistakes rerun

Rerun the pipeline for the current branch.

```sh
no-mistakes rerun
no-mistakes rerun --intent "the revised user goal"
```

Starts a new pipeline run from the current gate branch, except when the latest
terminal run has a verified unpublished head whose custody has not been
returned: rerun then uses that preserved terminal head even if the gate branch
is stale. After an adopt-preserved-head custody return, a known clean caller
whose changes the stale gate head does not retain also lets rerun select the
recorded preserved head instead of resuming the opposite lineage. The command
refuses instead of falling back to the gate branch when the run-specific
recovery ref is conflicting, invalid, or the recorded head is unavailable. Use
`no-mistakes axi status` and reconcile custody first in that case.
When invoked from a clean worktree, `rerun` also requires the selected gate or
preserved head to retain every change in that worktree's HEAD. Exact shared
history satisfies the check, as does an unambiguous ordered replay under new
commit SHAs proven by Git patch identity. New, missing, reversed, structurally
ambiguous, or otherwise unprovable changes refuse before starting or
superseding any run. The error names both full commit SHAs; inspect
`no-mistakes axi status` and follow its custody guidance, then use
`no-mistakes axi run` to submit local commits. The refusal leaves both branches
unchanged; rerun never replaces its selected head with the caller's head.
The same check applies to `axi run`'s rerun fallback after an up-to-date push.
Dirty worktrees and callers without clean-head evidence, including TUI reruns,
retain the existing selection behavior.
If the selected prior run has explicit intent, rerun inherits it exactly by default;
otherwise it performs fresh intent inference. `--intent` supplies a new canonical
explicit intent in either case. Inherited intent keeps distinct rerun provenance;
an override is recorded as newly supplied explicit intent, while fresh inference
records the transcript source. If another run is active on that branch, rerun
cancels it before starting over. Treat rerun as a between-runs action after a
failed or cancelled outcome; use `axi run` for separate local fixes, and do not
use rerun to bypass a gate.

| Flag | Type | Default | Description |
| ---- | ---- | ------- | ----------- |
| `--intent` | `string` | (none) | Explicit intent overriding inherited intent or fresh inference |

## no-mistakes sync

Freshly verify and, with confirmation, safely move the invoking branch to an exact pipeline-owned push binding.

```sh
no-mistakes sync
no-mistakes sync --check
no-mistakes sync --yes
no-mistakes sync --recover
no-mistakes sync --recover --keep-local
no-mistakes sync --release
no-mistakes sync --bind-archive-ref refs/heads/archive/<name>
```

| Flag                 | Type     | Default | Description                                                     |
| -------------------- | -------- | ------- | --------------------------------------------------------------- |
| `--check`            | `bool`   | `false` | Verify and print the fresh plan without changing `HEAD`         |
| `-y`, `--yes`        | `bool`   | `false` | Apply an eligible guarded synchronization, recovery, or release without an interactive prompt |
| `--recover`          | `bool`   | `false` | Return custody of a branch stranded by a terminal run with unpublished pipeline commits (a no-op when cancellation already released the branch) |
| `--keep-local`       | `bool`   | `false` | With `--recover`: keep the current local head; never touches the worktree |
| `--release`          | `bool`   | `false` | Release a terminal run's stale push binding whose live remote no longer matches it; see `axi sync --release` |
| `--bind-archive-ref` | `string` | (none)  | Bind one existing `refs/heads/archive/*` commit as exact keep-local recovery evidence without changing Git refs |

Without `--yes`, apply prints the exact full-SHA plan and requires TTY confirmation; `--recover` and `--release` prompt the same way before returning custody or releasing the binding. Archive binding is itself explicit, does not prompt, and cannot be combined with synchronization, recovery, or `--yes`.
A non-TTY apply, recovery, or release refuses with a direct `--yes` hint.
The command uses the same service and safety contract as `no-mistakes axi sync`, including the guarded equivalent advance and custody recovery documented there; it never stashes, rebases, creates a merge commit, switches branches, deletes a branch, or updates an external remote.

## no-mistakes status

Show repo, daemon, active run, and relevant cached local-branch synchronization status.

```sh
no-mistakes status
```

Displays:

- Repo path, upstream URL, and fork URL when configured
- Gate path
- Daemon status (running/stopped, PID)
- Active run details: ID, branch, status, head SHA, start time

## no-mistakes runs

List recorded pipeline runs for the current repo.

```sh
no-mistakes runs [--limit <n>]
```

| Flag      | Type  | Default | Description                       |
| --------- | ----- | ------- | --------------------------------- |
| `--limit` | `int` | `10`    | Maximum number of runs to display |

Shows runs newest-first with branch, status (styled), short SHA, timestamp, and PR URL if set.

## no-mistakes eval

Inspect the locally collected review-case corpus before spending tokens, replay an explicit agent and model with isolated no-mistakes state and a throwaway worktree, and report finding-level scores, token cost, wall time, and the recall-versus-cost frontier. Eligible cases are collected automatically as runs finish; `eval capture <run-id>` collects one on demand; `eval miss ingest <run-id> --finding '<json>'` labels a confirmed post-PR miss (review passed green, later caught) as false-negative gold.

See [Evaluation toolkit](/no-mistakes/reference/eval/) for the local-only boundary, collection and retention, command flags, label policy, and reporting semantics.

## no-mistakes stats

Show historical usage stats across all repos.

```sh
no-mistakes stats
```

Displays total changes, rescued changes, rescue rate, reported and fixed mistakes, fixes by pipeline step, and the top repos by rescue activity.
The Pushes section reports `Coalesced` (runs replaced by a newer push while still waiting under [`push_quiet_window`](/no-mistakes/reference/global-config/#push_quiet_window), each a mid-pipeline restart avoided) and `Superseded` (all other replacements, including reruns and explicit launches).

Use `--agents` for local, per-purpose agent performance aggregates: duration and the subprocess-vs-model time split, session mode, errors, the token totals (input, output, cache-read, cache-creation, fresh input, reasoning), and the model round-trip and tool-category activity histogram, with a `METRICS` coverage count that tells a real zero apart from missing instrumentation.
Use `--run <id>` to inspect the individual agent invocations for one run - including each invocation's per-round token deltas next to the raw counters (cumulative across a resumed session for codex; per-invocation for pi), tool-category breakdown, workload size, finding count, and fallback reason - plus the total time parked at approval gates; it implies `--agents`.
The combined Document/Lint invocation is labeled `housekeeping (document+lint)` and attributed to `document+lint`, making its shared duration and tokens explicit without adding a second agent call.
Nullable fields an adapter did not report, including raw input, output, and cache-read token counts, render as `-` (unknown), which is distinct from a recorded `0`.
A `--agents` token total is all-or-nothing: it reads `-` for the whole purpose unless every invocation in it reported that field, so one failed round that reported no usage leaves the group's total unknown instead of silently under-counted.

```sh
no-mistakes stats --agents
no-mistakes stats --run <id>
```

This detailed performance evidence stays local in `state.sqlite`; it is not sent to telemetry.
The field definitions and their local/remote split are owned by [the environment reference](/reference/environment/#what-stays-local-and-what-leaves-the-machine).

## no-mistakes doctor

Check system health and dependencies.

```sh
no-mistakes doctor
```

Checks:

- `git` binary
- `gh` CLI (optional, needed for GitHub PR and CI steps)
- `az` CLI (optional, needed for Azure DevOps PR and CI steps)
- Data directory (`~/.no-mistakes/`)
- SQLite database
- Daemon status
- Agent runners: [supported native binaries](/no-mistakes/reference/global-config/#agent_path_override), plus the optional ACP bridge `acpx`
- ACP alias default binaries: `cursor-agent` plus `acpx` for `cursor`
- Effective global agent configuration, reported as `gate validation`; an unavailable configured runner is a failed check because the gate cannot validate without it
- Every configured [`forge_profiles`](/no-mistakes/reference/global-config/#forge_profiles) entry, reported as `forge <host>`: the profile resolves and validates, its provider CLI is installed, and that CLI is authenticated for the profile's host

Uses indicators: `✓` (available), `–` (not found, optional), `✗` (problem detected).

The standalone runner rows inspect default binary names; the `cursor` row reports whichever of `cursor-agent` and `acpx` are missing.
The [Global Config Reference](/no-mistakes/reference/global-config/) owns ACP gate-validation availability and probing semantics.
Each validation run performs the authoritative agent resolution again after applying any trusted repository-level override.

`doctor` checks `gh` and `az` availability. [Provider Integration](/no-mistakes/guides/provider-integration/) owns the separate setup checks for GitLab, Forgejo, Bitbucket Cloud, Gitea, and the Azure DevOps extension and PAT.

`tea` stays docs-only like `glab`, `forgejo-axi`, and Bitbucket's env vars, rather than an active `doctor` check like `gh`/`az`: Gitea is almost always self-hosted, so a bare "`tea` not found" row would be a near-universal, low-value warning for the vast majority of users who have no Gitea instance at all.

## no-mistakes update

Update the installed binary and reset the daemon.

```sh
no-mistakes update
no-mistakes update --beta
no-mistakes update -y
no-mistakes update --force
```

Downloads the latest release, verifies the SHA-256 checksum, atomically replaces the running binary, and resets the daemon when it is running or stale daemon artifacts exist so the new executable is picked up, preferring the managed service path and falling back to a detached daemon if service startup is unavailable or fails.
By default this installs the latest stable release.
Pass `--beta` to include prereleases and install the latest beta when one is newer than the current stable release.
Version discovery reads a `channels.json` manifest from the GitHub release-asset CDN (`releases/download/channels/channels.json`) so it does not consume the unauthenticated REST API rate limit. If the manifest is unavailable or invalid, the update fails without falling back to the REST API.
If the daemon is running from a different executable path, update still prompts before replacing it; pass `-y`/`--yes` to answer that prompt non-interactively.
If the daemon executable path cannot be determined, the update aborts before replacement.
If the daemon does not come back cleanly after a successful replacement, the command reports that failure.
On macOS, removes the quarantine extended attribute.
[Daemon & Worktrees](/no-mistakes/concepts/daemon/#starting-and-stopping)
owns the active-run guard, the scope of `--force` and `--yes`, and recursive
validation-step containment.

Because `update` installs the latest official release binary, the replacement binary includes the default self-hosted telemetry host and website ID. Disable telemetry with `NO_MISTAKES_TELEMETRY=0`, or override the host and website ID with `NO_MISTAKES_UMAMI_HOST` and `NO_MISTAKES_UMAMI_WEBSITE_ID`.

Background update checks run automatically on each CLI invocation (except `update` itself and version queries `--version` / `-v`, which stay side-effect-free). If a newer version is available, a notification is printed to stderr. Suppressed for dev builds or when `NO_MISTAKES_NO_UPDATE_CHECK=1` is set.

## no-mistakes daemon start

Start the daemon, installing or refreshing the managed service when possible.

```sh
no-mistakes daemon start
```

Prefers the managed service path and falls back to a detached daemon if service install or startup is unavailable or fails. If the daemon is already running, the command refreshes a stale macOS `launchd` or Linux `systemd` service definition and restarts through the managed service; if the definition is unchanged, it reports that the daemon is already running. [Daemon & Worktrees](/no-mistakes/concepts/daemon/#starting-and-stopping) owns the startup readiness, timeout, fallback cleanup, and singleton lifecycle details.

## no-mistakes daemon stop

Stop the running daemon process.

```sh
no-mistakes daemon stop
no-mistakes daemon stop --force
```

[Daemon & Worktrees](/no-mistakes/concepts/daemon/#starting-and-stopping)
owns the active-run guard, the scope of `--force`, and recursive
validation-step containment.

This does not remove the managed service. A later `no-mistakes`, `no-mistakes daemon start`, `init`, `attach`, `rerun`, or `update` can start the daemon again through the same service manager when available, or as a detached daemon otherwise.

## no-mistakes daemon restart

Restart the daemon.

```sh
no-mistakes daemon restart
no-mistakes daemon restart --force
```

Stops the current daemon and starts it again. This works whether the daemon is currently running or not.
[Daemon & Worktrees](/no-mistakes/concepts/daemon/#starting-and-stopping)
owns the active-run guard, the scope of `--force`, and recursive
validation-step containment.

## no-mistakes daemon status

Check whether the daemon is running.

```sh
no-mistakes daemon status
```

Shows the PID if the daemon is running.

## Offline run confinement

```sh
no-mistakes axi run --isolate --intent "Review and validate the submitted head without integration or publication"
```

This opts one new run into the versioned offline confinement policy. The executor
dispatches **Review, Test, Document, Lint and every repository gate**, including
preparation and repair rounds, through the isolation worker. Each invocation has
a fresh namespace, native PostgreSQL cluster and constrained Deck model channel;
workers do not share a persistent namespace or disposable database. This is
broader than [`--isolate-test`](#run-scoped-test-isolation), which confines only Test.

The offline policy skips Rebase, Jev, Push, PR and CI with explicit reasons, rather
than integrating the base or using daemon credentials for those steps. Intent
execution is also skipped because the launch requires an explicit intent. The
submitted head is validated as-is. A completed confined run is **not** a published
PR or a CI validation; Test-preview publication is refused too. Daemon-owned launch
and recovery reads still fetch trusted configuration; the offline boundary is not
a promise that the daemon performs no network operations.

Launch uses a distinct RPC to import the verified local commit without a gate
push. Both CLI and daemon must support it; there is no ordinary-run fallback.
It requires a clean, checked-out feature worktree and explicit intent. An active
run is never superseded: reattachment requires its confinement pin and matching
submitted head, intent and base. `--base-branch` remains available, but `--isolate`
cannot combine with `--isolate-test`, skip, history, PR-association (including
retirement), or launch-receipt flags.

The run's policy pin survives repairs and daemon recovery; a mismatch refuses
worker dispatch instead of silently changing policy. Existing runs and new
ordinary runs or reruns do not acquire it. Request another confined run explicitly
with `--isolate`. Use normal status, gate-response and cancellation APIs for the
selected run; do not remove its pin to resume it unconfined.

The [environment prerequisites](/no-mistakes/reference/environment/#opt-in-test-isolation)
are shared with Test isolation, including the Linux-only substrate and Deck-only
adapter. Unsupported setups refuse rather than running steps on the host. General
network dependency downloads and external services are unavailable to workers;
required dependencies must already be accessible in permitted mounts. For policy
binding, handoff and recovery invariants, see the
[confinement contract](https://github.com/bastotec/no-mistakes/blob/main/docs/test-isolation-design.md#whole-run-confinement).
This command does not authorize shared-daemon activation or reconfiguration.

## Run-scoped Test isolation

```sh
no-mistakes axi run --isolate-test --intent "Validate this change without ambient credentials or general network access"
```

This explicitly opts **one new run's whole Test descendant tree** into
Linux/bubblewrap isolation with a fresh native PostgreSQL database and a
constrained Deck model channel. It does not sandbox other steps, enable existing
runs, configure a shared daemon, or activate by repository config. A new ordinary
run or `rerun` never inherits the opt-in; use `axi run --isolate-test` to request
another isolated run.

The launch imports the verified local commit through a distinct RPC, without a
gate push. An older daemon refuses; there is no fallback to unconstrained Test.
The invoking feature worktree must be clean. Existing active runs are never
silently replaced; reattachment requires the same opted-in run, submitted head,
intent and base. The flag cannot combine with skip, history, PR-association or
launch-receipt flags; `--base-branch` remains available.

Use the normal `axi status`, `axi respond` and cancellation APIs after launch.
Recovery preserves the per-run isolation pin and provisions a fresh database.
An interrupted started repair retains the existing repair-reconciliation gate;
it is not replayed as an ordinary agent invocation.

See [environment prerequisites](/no-mistakes/reference/environment/#opt-in-test-isolation)
and the implementation's invariants and bounded activation proposal in
[`docs/test-isolation-design.md`](https://github.com/bastotec/no-mistakes/blob/main/docs/test-isolation-design.md).
Shared activation requires separate approval; this command is not an instruction
to restart or update any running shared daemon.
