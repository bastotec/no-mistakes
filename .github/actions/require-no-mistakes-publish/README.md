# Ordered current-verdict publisher

This action separates **event evidence** from the **current PR verdict**. It
uses the existing `require-no-mistakes/verify.py` rules unchanged. Original
payloads retain their success/failure/exemption result in outputs, logs, and the
job summary; a stale failure is never relabeled compliant. The runner-owned job
reports whether recording/publication executed, not whether the PR is compliant.
Only the action-created **PR must be raised via no-mistakes** check is the
compliance verdict. A current invalid attestation publishes FAILURE.

## Caller contract

[`caller.yml.example`](caller.yml.example) is the complete migration template.
Install it with an **already-published immutable action SHA**, never `@main` or
code from the judged PR. This is a two-phase rollout: publish/test this action,
then change callers in a separate PR. The repository's existing immutable gate
pin intentionally does not change in the action's own implementation PR.

The entire action must run inside **one non-cancelling, per-PR job concurrency
group**, shared by every writer of this check. Do not put the lock around just
validation, add workflow-level cancellation, or run another publisher outside
it. GitHub concurrency is not FIFO and may replace pending jobs: every job that
runs rechecks authority, rather than assuming queue/start/completion order.
Coalesced jobs retain their archived event; they do not get a made-up verdict.
The latest pending event must be allowed to execute for liveness. A deliberately
cancelled latest publisher needs an operator rerun; cancellation is not approval.

Use `pull_request_target` with `pull-requests: read`, `actions: read`, and
`checks: write`. Ordinary fork `pull_request` tokens cannot write checks. This
privileged workflow must **never check out or execute PR code**, consume PR
artifacts/caches, or run contributor-supplied scripts. The pinned action reads
only the event and GitHub metadata. PR text is data, never shell interpolation.
Do not forward transformed bodies or head overrides: this publisher validates
the exact original event body and its exact head SHA. Exemptions are trusted
caller policy, identical to the legacy action's exemptions.

## Ordering protocol

Inside the caller's lock, the publisher:

1. Validates and records the original event snapshot, including failures.
2. Gets the workflow ID, `run_number`, and `run_attempt` from GitHub's run API
   and checks them against the executing run identity. Retries of an old event
   keep its old run number; attempts cannot jump ahead of a newer event.
3. Reads its existing check on this exact head. The check's `external_id` is a
   persisted `(PR, workflow, run_number, run_attempt)` watermark. Lower ranks
   cannot write, regardless of queue order, completion timestamps, or ABA edits
   that restore an earlier body. Equal ranks are idempotent after a lost write
   response. A conflicting workflow, malformed watermark, or multiple owned
   checks fails closed without overwriting anything.
4. Immediately before writing, compares the original **raw body and head SHA**
   against a fresh PR API read. Both must match exactly. Comparing head alone
   would miss the same-head edited-event race.
5. Creates one named check for that head, or updates the same check in place
   with the higher rank. The lock spans the watermark read, state read, and
   write. The failure path uses the same protocol as the success path.

GitHub does not offer an atomic compare-and-set spanning a PR body and a check
run. The lock serializes check writers, not edits to PR text. An edit arriving
between the last read and write schedules its own publisher; once that newer
event has published, no older event can supersede it. This is not a claim that
a webhook has zero delivery latency, nor that arbitrary other check writers are
coordinated. Read/write failures fail the execution job visibly; they do not
certify a new passing verdict or silently fall back to archived authority.

## Cutover

Keep the branch protection/ruleset check name unchanged and retain synchronize
triggers: every pushed head needs a verdict. Remove the legacy job with that
same check name; do not run both protocols concurrently. Cancel legacy queued
runs before enabling the new publisher and do not rerun archived legacy
workflows after cutover. A new protocol cannot fence a legacy runner-owned job
that does not participate in it. Prefer a fresh head at cutover so the old
runner-owned check cannot compete with the new named check on the same commit.
Do not waive an existing red check or merge red as part of migration.

A renamed/replaced workflow ID is not automatically authorized to reuse a
persisted watermark: migrate on a fresh head after retiring the old writer.
The action intentionally refuses conflicting identities on the same head.

## Regression evidence

`require_no_mistakes_publish_test.go` executes the actual publisher against a
local GitHub-shaped REST server. It starts a newer edited-event success before
releasing an older synchronize-event failure and checks the exact writes and
persisted current verdict. In-order failure, same-head body edits, changed
heads, restored bodies, reruns, idempotency, authority errors, and the caller's
fork/serialization boundary are tested without timing sleeps.
