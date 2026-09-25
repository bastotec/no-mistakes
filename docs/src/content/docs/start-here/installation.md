---
title: Installation
description: All install options, prerequisites, update, and uninstall.
---

## macOS / Linux

```sh
curl -fsSL https://raw.githubusercontent.com/kunchenguid/no-mistakes/main/docs/install.sh | sh
```

The installer keeps the real binary in `~/.no-mistakes/bin` and exposes `no-mistakes` through a symlink in `~/.local/bin` or `/usr/local/bin`. That keeps future `no-mistakes update` runs in a user-owned location instead of rewriting a system binary in place.

It also installs or refreshes the background daemon for you by running `no-mistakes daemon restart`, preferring a managed service (launchd on macOS, systemd user service on Linux) and falling back to a detached daemon if that path is unavailable. If the restart fails, the install command fails.

Official release binaries installed this way include the default self-hosted telemetry host and website ID. Disable telemetry with `NO_MISTAKES_TELEMETRY=0`, or override the host and website ID with `NO_MISTAKES_UMAMI_HOST` and `NO_MISTAKES_UMAMI_WEBSITE_ID`.

## Windows (PowerShell)

```powershell
irm https://raw.githubusercontent.com/kunchenguid/no-mistakes/main/docs/install.ps1 | iex
```

Installs the binary and restarts the background daemon automatically with `no-mistakes.exe daemon restart`, preferring a managed Task Scheduler task and falling back to a detached daemon if needed. If the restart fails, the install command fails.

Official release binaries installed this way include the default self-hosted telemetry host and website ID. Disable telemetry with `NO_MISTAKES_TELEMETRY=0`, or override the host and website ID with `NO_MISTAKES_UMAMI_HOST` and `NO_MISTAKES_UMAMI_WEBSITE_ID`.

## Go install

```sh
go install github.com/kunchenguid/no-mistakes/cmd/no-mistakes@latest
```

`go install` builds the CLI without an embedded telemetry website ID, so telemetry stays off by default unless you later set `NO_MISTAKES_UMAMI_WEBSITE_ID` at runtime.

## From source

```sh
git clone git@github.com:kunchenguid/no-mistakes.git
cd no-mistakes
make build
make install
```

`make build` stamps the version `no-mistakes --version` reports. A checkout sitting exactly at a release tag that carries a version number, with a clean tree, stamps that tag verbatim - the release path; a non-version exact tag such as `channels` is not a release. Any other build stamps the fork stamp `<upstream-semver>-fork-<sha>`, for example `1.83.1-fork-6911e03`: the newest release tag the clone knows (the upstream semantic version the fork tracks), then the fork marker, then the build's short commit. The comparable core passes every minimum-version check, so a from-source build proves its version floor instead of reporting `dev` and inviting an install over itself, and the whole stamp marks the binary as this fork's build rather than stock upstream. Outside a checkout, or in a shallow or tagless clone where no release tag can be found, it falls back to the `dev` sentinel.

Passing `VERSION=` by hand overrides that, and the build refuses a value with no version number in it (`VERSION=fork-7e84d0d`, `VERSION=dev`). A version string with nothing comparable in it makes every minimum-version check read the tool as not installed and offer to install over the build that is running. The refusal prints the fork stamp of the release your clone knows as the replacement - `make build VERSION=1.83.1-fork-7e84d0d` - or a `0.0.0` placeholder core to replace when the clone has no tag to read. Any comparable hand-passed value is still accepted as-is: `make build VERSION=v1.76.0+fork.7e84d0d` stamps itself verbatim.

A fork built that way is no longer a development build, so once the update channel carries a release the stamp does not outrank, the updater will say so; the fork stamp is a prerelease of the release it names, so that includes that release itself. `NO_MISTAKES_NO_UPDATE_CHECK=1` silences that notice, and taking the upstream release stays your decision.

`make build` embeds the telemetry host from `NO_MISTAKES_UMAMI_HOST` in a repo-local `.env` first, then `UMAMI_HOST` from the shell, then the default self-hosted host. It embeds the telemetry website ID from `NO_MISTAKES_UMAMI_WEBSITE_ID` in `.env` first, then `UMAMI_WEBSITE_ID` from the shell, then the default website ID.

## Prerequisites

- **git** - required
- **One supported agent runner** - see [Global Config](/no-mistakes/reference/global-config/#agent) for native agents and Cursor/ACP requirements
- **Optional, for PRs and CI:**
  - `gh` CLI (GitHub)
  - `glab` CLI (GitLab)
  - `NO_MISTAKES_BITBUCKET_EMAIL` and `NO_MISTAKES_BITBUCKET_API_TOKEN` (Bitbucket Cloud)
  - `az` CLI with the `azure-devops` extension (Azure DevOps)
  - `tea` CLI (Gitea)

Run `no-mistakes doctor` to check native agents, ACP aliases such as `cursor`, provider tools, and whether the configured global runner can start a validation gate.
Every validation gate requires a runnable pipeline agent and otherwise fails before its first pipeline step.

See [Provider Integration](/no-mistakes/guides/provider-integration/) for PR and CI setup per host.

## Update

```sh
no-mistakes update
no-mistakes update --beta
no-mistakes update -y
```

This downloads the latest release from GitHub, verifies the SHA-256 checksum, atomically replaces the binary, and resets the daemon so it picks up the new executable. It prefers the managed service path and falls back to a detached daemon if service startup is unavailable or fails.

`no-mistakes update` installs the latest stable release.
Use `no-mistakes update --beta` to opt into prereleases and install the latest beta when one is newer than the current stable release.
Use `no-mistakes update -y` to answer yes to the daemon-executable-mismatch prompt described below.
Version discovery uses the GitHub release-asset CDN rather than the rate-limited REST API; see [`no-mistakes update`](/no-mistakes/reference/cli/#no-mistakes-update).

Because `update` installs the latest official release binary, it installs a binary with the default self-hosted telemetry host and website ID. Disable telemetry with `NO_MISTAKES_TELEMETRY=0`, or override the host and website ID with `NO_MISTAKES_UMAMI_HOST` and `NO_MISTAKES_UMAMI_WEBSITE_ID`.

If pending or running pipeline runs exist, the update refuses to restart the daemon and prints each active run's ID, status, branch, and short head SHA. Pass `--force` to restart the daemon anyway and accept that those runs may fail; `-y`/`--yes` does **not** bypass this guard.
If the running daemon was started from a different binary, the update still prompts before replacing it; `-y`/`--yes` answers that prompt non-interactively.
If the daemon executable path cannot be determined, the update aborts before replacing the binary.
If the daemon does not come back cleanly after a successful replacement, the new binary stays installed but the command reports the daemon reset failure.

Background update checks run automatically on each CLI invocation (except `update` itself and version queries `--version` / `-v`, which stay side-effect-free). Suppress with `NO_MISTAKES_NO_UPDATE_CHECK=1`.

## Remove from a repo

```sh
no-mistakes eject
```

Removes the `no-mistakes` remote, deletes the bare repo, cleans up worktrees, and removes the database record.
It does not remove repo-local agent skill files created by `no-mistakes init`.

## Uninstall

Stop the daemon, delete the binary, and clear state:

```sh
no-mistakes daemon stop
rm -f ~/.local/bin/no-mistakes /usr/local/bin/no-mistakes
rm -rf ~/.no-mistakes
```

If you configured [`worktree_roots`](/no-mistakes/reference/global-config/#worktree_roots), also delete the run worktree directories it placed outside `~/.no-mistakes`.

On macOS, also remove `~/Library/LaunchAgents/com.kunchenguid.no-mistakes.daemon.*.plist`. On Linux, also remove `~/.config/systemd/user/no-mistakes-daemon-*.service`. On Windows, remove the `no-mistakes-daemon-*` Task Scheduler task.
