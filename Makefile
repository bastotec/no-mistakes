# A build stamps VERSION into `internal/buildinfo`, and that string is what
# every minimum-version check reads back out of `no-mistakes version`.  A
# version check cannot be asked to accept a label with no version number in
# it - it would then have to accept every label, including `dev`, and stop
# meaning anything - so a build that stamps one reports itself as NOT
# INSTALLED and invites the operator to install over the very binary that is
# running.  That is exactly what `VERSION=fork-7e84d0d` produced: a hand
# passed label discarded `v1.76.0-5-g7e84d0d`, the comparable version this
# repository already knew.  Mark a fork with semantic versioning's build
# metadata BESIDE the real version (`v1.76.0+fork.7e84d0d`), never instead of
# it.  Build metadata rather than a prerelease suffix: semver ranks a
# prerelease BELOW the release it names, so a prerelease-shaped fork of
# `v1.76.0` reads as older than `v1.76.0` and the updater offers to replace
# it with the release it was built from.
#
# A comparable version is a semver core of at least major.minor, optionally
# `v`-prefixed, with any prerelease/build metadata after it - what
# `git describe --tags` and every release tag produce, and what
# `internal/update`'s parseVersion accepts.  A bare abbreviated commit is
# deliberately not comparable.
VERSION_PATTERN := ^v?[0-9]+\.[0-9]+(\.[0-9]+)?(-[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?(\+[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?$$
comparable_version = $(shell printf '%s' '$(1)' | grep -Eq '$(VERSION_PATTERN)' && echo yes)

DESCRIBED_VERSION := $(shell git describe --tags --always --dirty 2>/dev/null)
# Loud normalization for the derived value: outside a checkout, or in a
# shallow/tagless clone where `git describe` can only report a commit, fall
# back to the `dev` sentinel that `internal/update` already understands
# rather than stamping a label that looks like a version and is not.  The
# rule this guard establishes is that the version slot never carries a label
# that looks like a version and is not, and a commit-ish produced by
# `git describe` violates it in exactly the same way a hand-passed one does,
# so refusing one while stamping the other would be inconsistent rather than
# narrower.  The sentinel costs no traceability, because the commit id is
# stamped independently in COMMIT and still appears in the version string's
# own commit field: nothing traceable is lost by the fallback, only the false
# appearance of a version.
VERSION ?= $(if $(call comparable_version,$(DESCRIBED_VERSION)),$(DESCRIBED_VERSION),dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
DATE    ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)

# A hand-passed VERSION (command line or environment) gets no sentinel
# fallback: refusing it is what keeps the broken outcome from silently
# becoming a shipped binary again.  The refusal is carried by the targets
# that stamp a binary, so an ambient VERSION nobody aimed at this build never
# kills `make clean`.
VERSION_REFUSED :=
ifneq ($(origin VERSION),file)
ifneq ($(call comparable_version,$(VERSION)),yes)
VERSION_REFUSED := yes
endif
endif
# The suggestion is the release core the repository already knows plus the
# commit as build metadata, never a prerelease suffix - the same shape the
# refusal is teaching.  With no release to name (a tagless or shallow clone,
# or outside a checkout) the core is the 0.0.0 placeholder rather than a
# plausible release number: the message has to print something that passes
# the guard, and a copy-pasted fabricated release would ship a binary
# claiming a version it never came from.  Shipped anyway, 0.0.0 fails a floor
# check instead of quietly passing one.  Only `git describe`'s own trailing
# `-<count>-g<sha>[-dirty]` is stripped, never the tag's prerelease: a fork of
# `v1.77.0-rc.1` must not be told to call itself the unreleased `v1.77.0`.
DESCRIBED_RELEASE := $(shell printf '%s' '$(DESCRIBED_VERSION)' | sed -E 's/-[0-9]+-g[0-9a-f]+(-dirty)?$$//; s/-dirty$$//')
FORK_RELEASE := $(if $(call comparable_version,$(DESCRIBED_RELEASE)),$(DESCRIBED_RELEASE),v0.0.0)
FORK_SUGGESTION := $(FORK_RELEASE)+fork.$(COMMIT)
PLACEHOLDER_NOTE := $(if $(call comparable_version,$(DESCRIBED_RELEASE)),, The 0.0.0 core there is a placeholder because no release tag was found here: replace it with the release your fork is built from.)
VERSION_REFUSAL := VERSION=$(VERSION) carries no comparable version number, so this build would report itself as not installed to every minimum-version check and invite an install over itself. Pass a comparable version such as VERSION=$(FORK_SUGGESTION), or omit VERSION to use git describe.$(PLACEHOLDER_NOTE)
DEFAULT_UMAMI_HOST := https://a.kunchenguid.com
DEFAULT_UMAMI_WEBSITE_ID := f959e889-92f5-4121-8a1f-571b10861198
DOTENV_UMAMI_HOST := $(shell [ -f .env ] && perl -ne 'next if /^\s*(?:\#|$$)/; s/^\s*export\s+//; next unless /^\s*NO_MISTAKES_UMAMI_HOST\s*=\s*(.*)$$/; $$v=$$1; $$v =~ s/^\s+|\s+$$//g; if ($$v =~ /^( ["\x27] )(.*)\1$$/x) { $$v=$$2; } else { $$v =~ s/\s+\#.*$$//; $$v =~ s/\s+$$//; } $$out=$$v; END { print $$out if defined $$out }' .env)
DOTENV_UMAMI_WEBSITE_ID := $(shell [ -f .env ] && perl -ne 'next if /^\s*(?:\#|$$)/; s/^\s*export\s+//; next unless /^\s*NO_MISTAKES_UMAMI_WEBSITE_ID\s*=\s*(.*)$$/; $$v=$$1; $$v =~ s/^\s+|\s+$$//g; if ($$v =~ /^(["\x27])(.*)\1$$/) { $$v=$$2; } else { $$v =~ s/\s+\#.*$$//; $$v =~ s/\s+$$//; } $$out=$$v; END { print $$out if defined $$out }' .env)
override UMAMI_HOST := $(if $(DOTENV_UMAMI_HOST),$(DOTENV_UMAMI_HOST),$(if $(UMAMI_HOST),$(UMAMI_HOST),$(DEFAULT_UMAMI_HOST)))
override UMAMI_WEBSITE_ID := $(if $(DOTENV_UMAMI_WEBSITE_ID),$(DOTENV_UMAMI_WEBSITE_ID),$(if $(UMAMI_WEBSITE_ID),$(UMAMI_WEBSITE_ID),$(DEFAULT_UMAMI_WEBSITE_ID)))
LDFLAGS := -X github.com/kunchenguid/no-mistakes/internal/buildinfo.Version=$(VERSION) \
           -X github.com/kunchenguid/no-mistakes/internal/buildinfo.Commit=$(COMMIT) \
           -X github.com/kunchenguid/no-mistakes/internal/buildinfo.Date=$(DATE) \
           -X github.com/kunchenguid/no-mistakes/internal/buildinfo.TelemetryHost=$(UMAMI_HOST) \
           -X github.com/kunchenguid/no-mistakes/internal/buildinfo.TelemetryWebsiteID=$(UMAMI_WEBSITE_ID)

# The guard target sits above `build`, so the default goal is named rather
# than left to target order: a bare `make` builds the binary.
.DEFAULT_GOAL := build
.PHONY: build dist install version-guard test e2e e2e-record lint fmt clean docs docs-build docs-preview demo skill skill-check

DIST_DIR ?= dist
INSTALL_BIN := $(shell go env GOPATH)/bin/no-mistakes

# `+` so the refusal is honoured under `make -n` too: a dry run that prints a
# stamping command line must not pretend a refused VERSION would build.
version-guard:
ifeq ($(VERSION_REFUSED),yes)
	+@printf '%s\n' '$(VERSION_REFUSAL)' >&2; exit 1
endif

build: version-guard
	go build -ldflags "$(LDFLAGS)" -o bin/no-mistakes ./cmd/no-mistakes

dist: version-guard
	rm -rf $(DIST_DIR)
	mkdir -p $(DIST_DIR)
	for target in darwin/amd64 darwin/arm64 linux/amd64 linux/arm64 windows/amd64 windows/arm64; do \
		os=$${target%/*}; \
		arch=$${target#*/}; \
		bin=no-mistakes; \
		out="$(DIST_DIR)/$$bin"; \
		if [ "$$os" = "windows" ]; then \
			bin="$$bin.exe"; \
			out="$(DIST_DIR)/$$bin"; \
		fi; \
		CGO_ENABLED=0 GOOS="$$os" GOARCH="$$arch" go build -ldflags "$(LDFLAGS)" -o "$$out" ./cmd/no-mistakes; \
		if [ "$$os" = "windows" ]; then \
			( cd "$(DIST_DIR)" && zip -q "no-mistakes-$(VERSION)-$$os-$$arch.zip" "$$bin" ); \
		else \
			tar -C "$(DIST_DIR)" -czf "$(DIST_DIR)/no-mistakes-$(VERSION)-$$os-$$arch.tar.gz" "$$bin"; \
		fi; \
		rm -f "$$out"; \
	done

install: version-guard build
	mkdir -p $(dir $(INSTALL_BIN))
	install -m 755 bin/no-mistakes $(INSTALL_BIN)
	$(INSTALL_BIN) daemon stop
	$(INSTALL_BIN) daemon start

test:
	go test -race ./...

# End-to-end suite: drives the real no-mistakes binary against a fake
# agent through the full push -> pipeline -> push journey for each
# e2e-covered agent backend, plus the step-local e2e tests that live
# next to the pipeline-step code (e.g. coverage provider journeys).
# Excluded from `make test` because it is behind the `e2e` build tag and
# rebuilds binaries on each run.
#
# scripts/e2e.sh owns temporary-daemon inventory + EXIT/INT/TERM reaping so
# an interrupted or timed-out go test child cannot leave detached e2e
# daemons behind. Keepalive shells are out of scope. A SIGKILL of the
# wrapper shell itself does not run its trap; next-run pre-reap recovers.
e2e:
	@bash scripts/e2e.sh

# Re-record fixtures from the real claude/codex/opencode/antigravity CLIs and overwrite
# internal/e2e/fixtures/. Spends real API quota — run only when the upstream
# wire format changes or when adding a new agent or flavour. Personal paths are
# scrubbed automatically; review the diff before committing.
e2e-record:
	go run ./cmd/recordfixture claude   --out internal/e2e/fixtures/claude
	go run ./cmd/recordfixture codex    --out internal/e2e/fixtures/codex
	go run ./cmd/recordfixture opencode --out internal/e2e/fixtures/opencode
	go run ./cmd/recordfixture antigravity --out internal/e2e/fixtures/antigravity

# Regenerate the committed agent skill (skills/no-mistakes/SKILL.md) from the
# internal/skill source of truth.
skill:
	go run ./cmd/genskill

# Fail if the committed skill has drifted from the generator. Wired into lint
# so CI catches a forgotten `make skill`.
skill-check:
	go run ./cmd/genskill --check

lint: skill-check
	go vet ./...

fmt:
	gofmt -w .

docs: docs-build

docs-build:
	cd docs && npm ci && npm run build

docs-preview:
	cd docs && npm run preview

demo: build
	vhs demo.tape
	ffmpeg -i demo_raw.gif -filter_complex "\
		[0:v]split[orig][zoom_src];\
		[zoom_src]crop=963:570:0:0,scale=1100:650:flags=lanczos[zoomed];\
		[orig]scale=1100:650:flags=lanczos[base];\
		[base][zoomed]overlay=0:0:enable='lt(t,4.04)',setpts=1.9*PTS,\
		split[s0][s1];\
		[s0]palettegen=max_colors=128[p];\
		[s1][p]paletteuse=dither=sierra2_4a\
	" -r 10 -y demo.gif
	ffmpeg -i demo_raw.gif -filter_complex "\
		[0:v]split[orig][zoom_src];\
		[zoom_src]crop=963:570:0:0,scale=1100:650:flags=lanczos[zoomed];\
		[orig]scale=1100:650:flags=lanczos[base];\
		[base][zoomed]overlay=0:0:enable='lt(t,4.04)',setpts=1.9*PTS\
	" -c:v libx264 -pix_fmt yuv420p -movflags +faststart -r 30 -y demo.mp4
	rm -f demo_raw.gif

clean:
	rm -rf bin/
