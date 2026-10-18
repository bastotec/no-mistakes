# Run-scoped Test isolation: pre-implementation invariants

Status: proposed contract, recorded before implementation. This document does
not describe an available feature or authorize activation of a shared daemon.

## Boundary

An explicitly opted-in run places the **entire Test execution** behind one
OS-enforced boundary before launching its first child. This includes preparation,
configured commands, evidence and correction agents, repair agents, their tools,
and grandchildren that detach from a process group. A wrapper around only
`commands.test`, environment filtering alone, and process reaping are insufficient.
Other steps and runs without the opt-in retain their existing execution path.

A separate Test worker process is preferable to changing the shared daemon's
network namespace or environment: isolation state must never be process-global in
the daemon. Any parent-side operations needed to persist results must be narrow,
explicit operations, not an unrestricted subprocess or network execution service.

## Environment allow/deny sets

Build the initial child environment from an empty map, never from `os.Environ`
with a denylist. The initial proposed allowlist contains only fixed `PATH`,
`LANG`, `HOME`, `TMPDIR`, `PWD`, the gate-role marker, Git noninteractive settings,
and generated connection settings for this invocation's disposable database and
model channel. HOME and temporary storage are fresh, private directories.

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
fixed configured upstream. Incoming credentials/cookies and redirects are
refused or discarded; the host-side bridge supplies the key. A key file under
any sandbox mount is refused. This transport preserves the native Deck
Responses API, not an invented Chat Completions compatibility layer.

### Durable state and recovery

A distinct launch RPC imports the verified local head without pushing a gate
hook; an older daemon refuses it before an ordinary run can start. The immutable
`run_test_isolation` row enables only that run. A new run or rerun needs its own
explicit opt-in; restarting the daemon retains an existing run's pin.

The worker receives a disposable snapshot of only its run's advisory database
records, with remote credentials and raw config provenance removed. The live
daemon database and sockets are not mounted. Worker head updates require a
parent acknowledgement; a repair's whole-step invocation is recorded under the
existing durable repair-custody protocol, never reconstructed as an unconfined
agent turn. An interrupted started repair follows the existing reconciliation
gate rather than blindly replaying its side effects.

Namespace teardown kills detached descendants and discards PostgreSQL storage.
The invocation-owned bridge is closed, and successful/error cleanup removes its
scratch snapshot and policy files. After a daemon crash, the next invocation
cleans its own stale scratch directory and creates a new cluster and channel;
it never imports prior disposable database contents. This does not clean
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
