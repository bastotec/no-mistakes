# Publication refresh evidence and limits

## Baseline audit

The audited baseline is `997828ed`, not an earlier review draft. The checks below distinguish code-level repairs already present at that baseline from claims that still require evidence. They do not certify any retained run or PR, and no retained run or PR was modified to obtain this evidence.

| Concern | Disposition at the audited baseline | Direct evidence |
| --- | --- | --- |
| R1: publishing after the remote source moved away from the recorded head | Already addressed for terminal continuation; not a new fix in this change | `internal/daemon/republish.go:168-175` reads the remote source with `git.LsRemote` and refuses inequality before creating the publication worktree. `TestRepublishPublication_RefusesIneligibleRuns` includes moved-remote refusal. |
| R2: invoking unrestricted PR rendering could publish an orphan evidence branch or upload fresh media | Already constrained for terminal continuation; not a new fix in this change | `internal/daemon/republish.go:225-230` explicitly disables both evidence storage and media attachment on the continuation config. `internal/pipeline/steps/evidence_publish.go:36-39` returns before `evidence.Publish` when storage is disabled. This is a control-flow argument, not a claim that a paid provider upload was exercised. |
| R3: recovered-config and PR-policy reads could mutate shared fetch state | Already uses a dedicated config loader and private refs; not a new fix in this change | `internal/daemon/republish.go:219-222,277-279,340-385` uses `loadRepublishConfig`, a URL fetch into private refs, and an integration namespace. `internal/pipeline/steps/common_git.go:289-296` routes policy fetches through that namespace. The existing continuation fixture compares gate refs before/after and checks cleanup. No concurrent production contention experiment was performed; absence of all possible lock contention remains unproven. |
| Re-review target binding: live config could redirect historical publication | Already addressed for terminal continuation | `internal/daemon/republish.go:95-107,231-236` refuses an absent recorded target and installs the run's recorded target, rather than selecting a new config target. `TestRepublishPublication_RefusesIneligibleRuns` includes the missing-pin case. Target **commit** freshness remains the ordinary PR policy behavior; this does not certify historical validation against a newly moved target tip. |

The merged `ad930085` disposition helper (`internal/pipeline/review_disposition.go`) supplements a **new review assessment** with unresolved findings and unreviewed-fix uncertainty. It is not evidence that a later CI repair fixed an old review claim. Reusing it to rewrite historical review rows would misattribute a new conclusion to the original reviewer.

## Operation reference

The [`axi refresh-publication` reference](src/content/docs/reference/cli.md#no-mistakes-axi-refresh-publication) owns eligibility, preservation guarantees, disposition semantics, and provider-update limitations.

## Runnable synthetic evidence

- `go test ./internal/daemon -run '^TestRefreshPublication_RPCMonitorJourney$' -count=1` runs the registered publication-refresh RPC over a disposable IPC server into an actual executor and active CI monitor, with local Git transport and a fake `gh` executable. Its cases inspect the response and published body, check exact binding refusals and non-passing observations, compare recorded run/step/round history, and reject non-body forge mutations. It seeds a published repair rather than replaying a validation pipeline or performing a real CI repair.
- `go test -race ./internal/pipeline ./internal/pipeline/steps -run '^TestRefreshPublication' -count=1` exercises dispatch/serialization, cancellation, non-monitor refusals, real temporary local Git remotes, mocked forge identity/check/content adapters, exact binding refusals before writes, head movement during preparation, body races, edited ownership blocks, size limits, history preservation, and unreadable/empty/pending/failed/skipped check snapshots.
- `go test ./internal/daemon -run '^TestRepublishPublication' -count=1` reuses the existing synthetic terminal-continuation fixtures. It is not a replay of any retained validation run.

These establish the scoped synthetic behavior, not live provider behavior. A final provider race cannot be eliminated by a client-side recheck because body update lacks atomic head/body preconditions. No live consumer, paid provider workflow, shared-daemon installation/restart, or historical PR-body rewrite is part of this audit.
