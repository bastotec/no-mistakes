# Run-scoped isolation

Status: implemented as an explicit per-run opt-in. The invariants below were
recorded before implementation; the selected implementation and activation
limits describe the current contract. This does not authorize shared daemon
activation or additional provider or application runs.

## Boundary

An explicitly opted-in run places the **entire Test execution** behind one
OS-enforced boundary before launching its first child. This includes preparation,
configured commands, evidence and correction agents, repair agents, their tools,
and grandchildren that detach from a process group. A wrapper around only
`commands.test`, environment filtering alone, and process reaping are insufficient.
Other steps retain their existing execution path under `--isolate-test`; the
separate [whole-run confinement](#whole-run-confinement) opt-in extends this
boundary to additional steps. Runs without either opt-in remain unchanged.

A separate Test worker process is preferable to changing the shared daemon's
network namespace or environment: isolation state must never be process-global in
the daemon. Any parent-side operations needed to persist results must be narrow,
explicit operations, not an unrestricted subprocess or network execution service.

## Whole-run confinement

`--isolate` extends the same substrate through the daemon executor's dispatch
boundary, not a second sandbox implementation. The [CLI reference](src/content/docs/reference/cli.md#offline-run-confinement)
owns its step scope, skipped operations, launch restrictions and user-visible
limits. The Test-only opt-in remains separate.

`internal/runconfine/policy.go` owns the closed policy contract. Version 1 has
scope `all-confinable-steps` and an offline posture. Its canonical serialization
is hashed with the run ID and immutable submitted head, then persisted in
`run_confinement` before pipeline execution. Before each worker dispatch the
parent re-derives the digest against that submitted head, not the current head:
additive repairs may advance HEAD without changing authority. Missing or
mismatched pins, unreadable state, unsupported policy versions and unavailable
workers fail closed. Scope changes require a policy-version change; recovery
must not reinterpret an existing pin. There is no global/repository switch and
new reruns do not inherit this pin.

The worker reconstructs the selected core step or trusted custom gate using
narrowed configuration and an advisory database snapshot, never daemon sockets,
forge profiles or host agent overrides. Review's model selection is constrained
as documented under [`review.routes`](src/content/docs/reference/global-config.md#reviewroutes).
Document and Lint remain separate invocations, rather than sharing an in-memory
housekeeping result across namespaces. Each invocation uses the same host event
transport and worker head-export/acknowledgement and terminal-index protocol as
Test isolation. Head acknowledgement precedes the worker's recorded-head advance;
terminal completion still requires validated handoffs and worker exit. The
existing interrupted-repair custody and replay rules apply, not an unconfined
agent replay.

Offline contract canaries live in `internal/runconfine/policy_test.go`,
`internal/pipeline/run_confinement_test.go`, and
`internal/pipeline/steps/run_confinement_canary_test.go`; shared worker protocol
regressions live in `internal/pipeline/steps/isolated_worker_test.go` and
`test_isolation_contract_test.go`. These do not substitute for a running-daemon
launch/restart journey.

## Environment allow/deny sets

Build the initial child environment from an empty map, never from `os.Environ`
with a denylist. The [environment reference](src/content/docs/reference/environment.md#opt-in-test-isolation)
owns the worker's allowlist and generated connection settings. HOME and temporary
storage are fresh, private directories.

Do not inherit cloud, forge, provider, proxy, SSH, database, package-registry,
agent-session, shell-startup, Git-config, or application credentials. In particular,
no arbitrary run environment overlay may add an exception. Tests must inject
both familiar credential variables and an unknown sentinel and check a detached
grandchild, not only the immediate launcher.

Do not mount the operator's home, host `/proc`, host temporary directories,
credential sockets, Docker socket, daemon socket, or host network namespace.
Mount only required runtime files and run-owned inputs/storage. Repository bytes
are not automatically sanitized; repository-contained credentials are not made
safe by deleting environment variables. No connection to their services is allowed.

## Egress and database

Use a fresh network namespace per Test invocation, including retries and recovery.
It has loopback only, no default route, DNS transport, host networking, or network
administration capabilities available to the Test descendants. The namespace
contains one newly provisioned disposable database listener at a fixed, documented
loopback address and port. Do not forward a caller-supplied database URL, reuse an
existing server, or accept a provider/customer endpoint as configuration.

Other host-loopback ports, external IPv4/IPv6 addresses, and DNS must be unreachable.
Unix socket mounts must not provide an escape to arbitrary host networking.
Regression evidence must include a real database transaction, a host-loopback
canary denied from a detached grandchild, and denied external-address connection
attempts that do not send traffic outside the namespace.

## Explicit agent-model channel

A daemon-owned, invocation-scoped bridge is the sole network exception. Its host
side holds the model credential; no provider credential reaches Test environment,
filesystem, command line, or a tool subprocess. Expose only a fixed model-request
operation to one preselected endpoint, never CONNECT, arbitrary URLs, redirects,
forwarded authorization, or a general HTTP proxy. Strip incoming authorization
and reject unexpected paths/methods. Do not expose the gateway's administration
API or other provider services.

The model client reaches this bridge through a documented namespace-local
transport. It does not gain access to host loopback. Descendants can use only the
same bounded model operation, not recover its credential. A local deterministic
streaming model fixture proves actual adapter/tool operation without a provider
or application run; denied paths and redirect behavior need negative tests.

## Lifecycle and failure posture

Refuse an opted-in Test before launching product commands if any isolation,
credential, database, mount, or transport prerequisite cannot be established.
Never silently degrade to ordinary Test, including on unsupported operating
systems, unsupported adapters, namespace failure, or daemon recovery.

Persist the run opt-in before launch. Recovery must preserve that choice, destroy
or refuse stale resources, and establish a fresh disposable database; it must not
resume a pre-crash database or model bridge. Ordinary pre-feature records remain
non-opted-in. No global or repository setting may turn existing runs on.

Normal completion, failure, timeout, cancellation, and worker/daemon death close
the bridge, kill the namespace's descendants, and remove owned database storage.
Never change host firewall rules, shared environment, or shared database state.
Cleanup must not kill unrelated processes. Demonstrate cleanup after a detached
child and an interrupted worker; host networking and the daemon's original
credential environment must be unchanged afterwards.

## Selected implementation

The supported opt-in is Linux amd64/arm64 with bubblewrap, Landlock network ABI
4 or newer, and a non-cgo no-mistakes worker (`CGO_ENABLED=0`). Other systems,
missing native database tools, unsupported adapters, unavailable kernel policy,
and failed provisioning refuse isolation; none falls back to ordinary Test.
The normal release build already disables cgo. Deck is the only supported
isolated agent, and its override arguments/hooks are not imported into the worker.

### Original-client-compatible database

The actual runtime provisions **native PostgreSQL**, not an emulated SQL protocol.
An operator supplies a read-only installation prefix containing executable
`bin/initdb`, `bin/postgres`, and `bin/psql`, together with their runtime libraries
and share files. No system installation, download, container daemon, host
listener, existing database, or caller database URL is used by provisioning.

Each invocation initializes a fresh cluster and database in private `/tmp` tmpfs,
including repair rounds and recovery. The unprivileged namespace identity is
`nm_test`; the fixed DSN is:

```text
postgresql://nm_test@127.0.0.1:17681/nm_test?sslmode=disable
```

Both `DATABASE_URL` and `NM_TEST_DATABASE_URL` contain that generated DSN.
Ordinary PostgreSQL/Prisma clients keep their original protocol; clients must
use the supplied DSN instead of an ambient application/provider connection.
The disposable cluster uses trust authentication only inside the isolated
namespace. There is no host socket, password, or remotely exposed listener.
Provisioning commands alone receive a generated `LD_LIBRARY_PATH` pointing at
the read-only installation libraries, not a value inherited from the daemon.

The custom HTTP/SQLite listener in `internal/testisolation/testfixture` is **only
a regression fixture** for confinement. It is not linked into the production
worker, not a runtime fallback, not PostgreSQL/MySQL wire-compatible, and not
proof of production database acceptance. Native PostgreSQL transactions and a
real isolated-daemon journey separately establish the runtime database path.
No application acceptance run or provider call is part of those fixtures.

### Network enforcement and model transport

The worker runs in fresh mount, PID, user and network namespaces. Landlock
limits TCP bind/connect to ports 17681 (database) and 17682 (model front end) on
all worker threads and descendants. The network namespace has only loopback;
there is no route to the host or external addresses. An inherited seccomp socket
filter denies UDP, raw sockets, Unix path sockets, other socket families, MPTCP,
and Unix datagram socketpairs. Private Unix stream socketpairs remain usable by
clients; they do not expose host socket paths. Detached grandchildren inherit
both policies and cannot remove them.

The model channel is one preconnected Unix stream inherited by the worker,
then closed-on-exec before launching tools. There is no mounted host socket.
Deck reaches the namespace-local HTTP front end at `http://127.0.0.1:17682/v1`.
Only `POST /v1/responses` for the daemon-selected model is forwarded to the
fixed configured upstream. The bridge requires exactly one lowercase `model`
field matching that selection, rejects duplicate or case-variant model keys,
and canonicalizes the request with the model pinned before forwarding it with
the host credential. Incoming credentials/cookies and redirects are refused or
discarded; the host-side bridge supplies the key. A key file under
any sandbox mount is refused. This transport preserves the native Deck
Responses API, not an invented Chat Completions compatibility layer.

### Durable state and recovery

A distinct launch RPC imports the verified local head without pushing a gate
hook; an older daemon refuses it before an ordinary run can start. The immutable
`run_test_isolation` row enables only that run; restarting the daemon retains
that pin. See [run-scoped Test isolation](src/content/docs/reference/cli.md#run-scoped-test-isolation)
for launch, reattachment and new-run opt-in semantics.

The worker receives a disposable snapshot of only its run's advisory database
records, with remote credentials and raw config provenance removed. The live
daemon database and sockets are not mounted. Config provenance is excluded before
snapshot insertion and from prior-branch decision input, not deleted after export.

Shared gate Git metadata is never mounted. Each worker receives an independent
object/ref/index store with a generated configuration and resolved Git
author/committer identity. To preserve checkout and staging normalization, it also
copies the effective scalar checkout settings selected by `PrepareGit`
(`internal/testisolation/git.go`) and the external/default and `info/attributes`
bytes into private files; repository `.gitattributes` keeps its normal precedence.
An attributes path rooted in the run worktree must remain beneath that root even
through file or parent-directory symlinks, on every handoff. Escaping paths
refuse import rather than copying host bytes into the next worker. Explicit
operator-selected absolute paths outside the worktree remain supported.
Executable Git configuration is not copied. It transfers the run head's reachable
history, the first usable integration ref and its reachable history (or the
recorded base when no integration ref is usable), and objects referenced by the
incoming index. Unrelated refs and their otherwise unreachable objects, including
other runs' recovery refs, are not copied. The worktree's Git pointer is overmounted
read-only; shared configuration files and hooks are not mounted. A head update
sends an object pack, not a metadata path: the parent checks its objects and
ancestry, then advances only the detached run HEAD and index before acknowledging
it.

Opted-in Test admits only a single repository: submodules/gitlinks and nested
repositories (including `.git` files and bare layouts) are unsupported. Admission
checks HEAD/index metadata and uses a bounded, no-follow filesystem inspection
before launch, handoff acceptance, custody capture/restore and cleanup. Isolated
custody status never recurses into nested repositories. Unsupported or ambiguous
work is retained in place with durable custody; it is not reset, cleaned, deleted
or accepted as a completed handoff. Inspection is limited to 100,000 entries and
128 directory levels; exceeding either bound refuses admission. Ordinary runs
retain their existing repository and custody behavior.

For both isolation opt-ins, host-side worktree creation first registers a
no-checkout worktree, then suppresses callbacks using the destination's effective
configuration before materializing files: worktree-specific conditional includes
must not introduce an executable filter after suppression. Setup and recovery
fetches, fix-review diffs, Git handoffs, interrupted-work capture/restore, cleanup
inspection and terminal recovery anchoring also suppress executable callbacks
(hooks, filesystem monitors, clean/smudge/process filters and textconv where
applicable). Worker-writable scripts must not execute on the host at these
boundaries. Overrides are invocation-local; ordinary runs retain their existing
callback behavior. `internal/git/callbacks.go` owns suppression and
`internal/pipeline/survived_work.go` owns custody policy. Regressions include
`internal/git/callbacks_test.go`, `internal/daemon/manager_trust_test.go`,
`internal/daemon/stepdiff_test.go`, `internal/testisolation/git_test.go` and
`internal/pipeline/isolated_test_repair_test.go`; Test-only running-daemon boundary
journeys live in `internal/e2e/test_isolation_boundaries_test.go`.

Worker errors retain Test timeout and protected-path classifications, including
terminal repair replay, so interrupted edits keep their existing custody and
approval behavior. Privacy-safe invocation records and lifecycle activity return
to the parent; no live database or raw agent output is needed for accounting.
A repair's whole-step invocation is recorded under the existing durable
repair-custody protocol, never reconstructed as an unconfined agent turn. An interrupted started repair follows the existing reconciliation
gate rather than blindly replaying its side effects. Successful index import is
not completion: the final outcome and handoffs must validate and the namespace
worker must exit first. Parent cancellation before durable completion leaves the
repair unresolved, never cached as an ordinary worker failure. Isolated repair
completion commits its terminal result and removal of the matching active step
PID in one ownership-checked transaction. Restart therefore sees either an
unresolved repair requiring reconciliation or a replayable terminal result, not
a terminal failure fabricated from daemon shutdown or a stale active PID.

Namespace teardown kills detached descendants and discards PostgreSQL storage.
The invocation-owned bridge is closed, and successful/error cleanup removes its
scratch snapshot and policy files after the final index handoff. Staged-only
repair bytes are imported as Git objects and index entries without checking out
files, and copied into the next authorized worker even when no commit reaches
them, including after a protected-path refusal. An unexpected worker/daemon death
retains private Git state for reconciliation instead of deleting it or reusing
it as another invocation.
A subsequent authorized invocation needs fresh storage, a new cluster and a new
channel; it never imports prior disposable database contents. This does not clean
intentional worktree edits or roll back application commits.

## Bounded activation proposal (not activation approval)

This change installs no service and alters no running daemon. Shared activation
requires a separate explicit approval. The supported request command is:

```sh
NM_HOME="$isolated_home" "$binary" axi run --isolate-test --intent "$intent"
```

Before that command, the isolated daemon must already be running the compatible
non-cgo binary, with the native installation and model references described in
the [environment reference](src/content/docs/reference/environment.md#opt-in-test-isolation).
The model key must be outside the worktree, gate, evidence and runtime mounts.
The isolated root and all fixture/build scratch belong to the validation
worktree; do not point these commands at a shared service root.

Affected runs: only new runs explicitly launched with `--isolate-test`, and the
recovery/repair invocations of those same runs. Existing and ordinary pipelines
retain their environment and network behavior. There is no repository/global
switch that retroactively enables isolation. Required packages/caches and any
product binaries must already be available in the permitted mounts: arbitrary
network dependency installation, external previews, cloud services and customer
endpoints are intentionally unavailable inside isolated Test.

Rollback: cancel the specific opted-in run through the ordinary run API and
start a new ordinary run without the flag once custody is returned. Do not remove
an active run's isolation pin or resume it unconfined. No host firewall changes,
shared database mutations, or shared daemon restart are required. A separately
approved later binary rollout/revert must respect the normal active-run and
repair-custody lifecycle; this proposal does not authorize one.
