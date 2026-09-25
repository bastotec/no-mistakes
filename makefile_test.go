package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/testgit"
)

func TestMakeBuildPrioritizesDotEnvUmamiWebsiteID(t *testing.T) {
	skipMakeBuildTestsOnWindows(t)

	makePath, err := exec.LookPath("make")
	if err != nil {
		t.Skip("make not available")
	}

	workDir := writeTestMakeWorkspace(t)
	if err := os.WriteFile(filepath.Join(workDir, ".env"), []byte("NO_MISTAKES_UMAMI_WEBSITE_ID=website-from-dotenv\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	output := runMakeDryBuild(t, makePath, workDir, map[string]string{
		"UMAMI_WEBSITE_ID": "website-from-env",
	})

	if !strings.Contains(output, "TelemetryWebsiteID=website-from-dotenv") {
		t.Fatalf("make build output should embed .env website id, got:\n%s", output)
	}
	if strings.Contains(output, "TelemetryWebsiteID=website-from-env") {
		t.Fatalf("make build output should not prefer env website id when .env exists, got:\n%s", output)
	}
}

func TestMakeBuildUsesEnvUmamiWebsiteIDWhenDotEnvMissing(t *testing.T) {
	skipMakeBuildTestsOnWindows(t)

	makePath, err := exec.LookPath("make")
	if err != nil {
		t.Skip("make not available")
	}

	workDir := writeTestMakeWorkspace(t)
	output := runMakeDryBuild(t, makePath, workDir, map[string]string{
		"UMAMI_WEBSITE_ID": "website-from-env",
	})

	if !strings.Contains(output, "TelemetryWebsiteID=website-from-env") {
		t.Fatalf("make build output should embed env website id when .env is absent, got:\n%s", output)
	}
}

func TestMakeBuildEmbedsDefaultSelfHostedTelemetryConfig(t *testing.T) {
	skipMakeBuildTestsOnWindows(t)

	makePath, err := exec.LookPath("make")
	if err != nil {
		t.Skip("make not available")
	}

	workDir := writeTestMakeWorkspace(t)
	output := runMakeDryBuild(t, makePath, workDir, nil)

	if !strings.Contains(output, "TelemetryHost=https://a.kunchenguid.com") {
		t.Fatalf("make build output should embed default telemetry host, got:\n%s", output)
	}
	if !strings.Contains(output, "TelemetryWebsiteID=f959e889-92f5-4121-8a1f-571b10861198") {
		t.Fatalf("make build output should embed default telemetry website id, got:\n%s", output)
	}
}

func TestMakeBuildPrioritizesDotEnvUmamiHost(t *testing.T) {
	skipMakeBuildTestsOnWindows(t)

	makePath, err := exec.LookPath("make")
	if err != nil {
		t.Skip("make not available")
	}

	workDir := writeTestMakeWorkspace(t)
	if err := os.WriteFile(filepath.Join(workDir, ".env"), []byte("NO_MISTAKES_UMAMI_HOST=https://dotenv.example\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	output := runMakeDryBuild(t, makePath, workDir, map[string]string{
		"UMAMI_HOST": "https://env.example",
	})

	if !strings.Contains(output, "TelemetryHost=https://dotenv.example") {
		t.Fatalf("make build output should embed .env telemetry host, got:\n%s", output)
	}
	if strings.Contains(output, "TelemetryHost=https://env.example") {
		t.Fatalf("make build output should not prefer env telemetry host when .env exists, got:\n%s", output)
	}
}

func TestMakeBuildIgnoresUnrelatedDotEnvEntries(t *testing.T) {
	skipMakeBuildTestsOnWindows(t)

	makePath, err := exec.LookPath("make")
	if err != nil {
		t.Skip("make not available")
	}

	workDir := writeTestMakeWorkspace(t)
	if err := os.WriteFile(filepath.Join(workDir, ".env"), []byte("VERSION=from-dotenv\nNO_MISTAKES_UMAMI_WEBSITE_ID=website-from-dotenv\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	output := runMakeDryBuild(t, makePath, workDir, nil)

	if !strings.Contains(output, "TelemetryWebsiteID=website-from-dotenv") {
		t.Fatalf("make build output should still embed dotenv website id, got:\n%s", output)
	}
	if strings.Contains(output, "/internal/buildinfo.Version=from-dotenv") {
		t.Fatalf("make build should ignore unrelated dotenv entries, got:\n%s", output)
	}
}

func TestMakeBuildStripsInlineCommentsFromDotEnvUmamiWebsiteID(t *testing.T) {
	skipMakeBuildTestsOnWindows(t)

	makePath, err := exec.LookPath("make")
	if err != nil {
		t.Skip("make not available")
	}

	workDir := writeTestMakeWorkspace(t)
	if err := os.WriteFile(filepath.Join(workDir, ".env"), []byte("NO_MISTAKES_UMAMI_WEBSITE_ID=website-from-dotenv # dev\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	output := runMakeDryBuild(t, makePath, workDir, nil)

	if !strings.Contains(output, "TelemetryWebsiteID=website-from-dotenv") {
		t.Fatalf("make build output should strip inline comments from dotenv website id, got:\n%s", output)
	}
	if strings.Contains(output, "TelemetryWebsiteID=website-from-dotenv # dev") {
		t.Fatalf("make build output should not embed inline comments in website id, got:\n%s", output)
	}
}

func TestMakeBuildPreservesQuotedHashInDotEnvUmamiWebsiteID(t *testing.T) {
	skipMakeBuildTestsOnWindows(t)

	makePath, err := exec.LookPath("make")
	if err != nil {
		t.Skip("make not available")
	}

	workDir := writeTestMakeWorkspace(t)
	if err := os.WriteFile(filepath.Join(workDir, ".env"), []byte("NO_MISTAKES_UMAMI_WEBSITE_ID=\"website # dev\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	output := runMakeDryBuild(t, makePath, workDir, nil)

	if !strings.Contains(output, "TelemetryWebsiteID=website # dev") {
		t.Fatalf("make build output should preserve quoted hashes in dotenv website id, got:\n%s", output)
	}
	if strings.Contains(output, "TelemetryWebsiteID=\"website") {
		t.Fatalf("make build output should not truncate quoted dotenv website id, got:\n%s", output)
	}
}

// A hand-passed VERSION carrying no comparable version number is what
// produced `no-mistakes version fork-7e84d0d (...)`: a binary every
// minimum-version check reads as not installed, and then offers to install
// over. The build refuses it rather than shipping it.
func TestMakeBuildRefusesHandPassedVersionWithoutAComparableVersionNumber(t *testing.T) {
	skipMakeBuildTestsOnWindows(t)

	makePath := lookupMake(t)
	workDir := writeTestMakeWorkspace(t)

	// A multi-segment core is uncomparable for the same reason: it looks
	// like a version but `internal/update`'s parseVersion rejects it, so the
	// guard must not accept what the version check cannot read.
	for _, version := range []string{"fork-7e84d0d", "7e84d0d", "dev", "fork", "1.2.3.4", "v2026.01.15.3", "1.2.3-rc.", "1.2.3-rc..1", "1.0-o'brien"} {
		t.Run(version, func(t *testing.T) {
			output := runMakeBuildExpectingRefusal(t, makePath, workDir, []string{"build", "VERSION=" + version}, nil)

			if !strings.Contains(output, "carries no comparable version number") {
				t.Fatalf("make build should refuse VERSION=%s with an explanation, got:\n%s", version, output)
			}
			if !strings.Contains(output, "Pass a comparable version such as VERSION=") || !strings.Contains(output, "or omit VERSION to stamp this build's default version") {
				t.Fatalf("make build should suggest the fork stamp and the omit alternative to VERSION=%s, got:\n%s", version, output)
			}
			if strings.Contains(output, "/internal/buildinfo.Version="+version) {
				t.Fatalf("make build should not stamp VERSION=%s, got:\n%s", version, output)
			}
		})
	}
}

// The environment is a hand-passed VERSION too; `?=` would otherwise take it
// silently.
func TestMakeBuildRefusesUncomparableVersionFromTheEnvironment(t *testing.T) {
	skipMakeBuildTestsOnWindows(t)

	makePath := lookupMake(t)
	workDir := writeTestMakeWorkspace(t)

	output := runMakeBuildExpectingRefusal(t, makePath, workDir, []string{"build"}, map[string]string{"VERSION": "fork-7e84d0d"})

	if !strings.Contains(output, "carries no comparable version number") {
		t.Fatalf("make build should refuse an uncomparable VERSION from the environment, got:\n%s", output)
	}
}

// Marking a fork is fine as long as the marker rides beside a comparable
// version instead of replacing it - semantic versioning's build metadata.
// The release path (a plain tag) has to keep working exactly as it does.
func TestMakeBuildAcceptsComparableVersions(t *testing.T) {
	skipMakeBuildTestsOnWindows(t)

	makePath := lookupMake(t)
	workDir := writeTestMakeWorkspace(t)

	for _, version := range []string{"v1.76.0", "1.76.0", "v1.76.0-5-g7e84d0d", "v1.76.0-5-g7e84d0d-dirty+fork", "v1.76.0+fork.7e84d0d", "v2026.1.15", "1.76.0-rc.1"} {
		t.Run(version, func(t *testing.T) {
			output := runMakeDryBuild(t, makePath, workDir, map[string]string{"VERSION": version})

			if !strings.Contains(output, "/internal/buildinfo.Version="+version) {
				t.Fatalf("make build should stamp VERSION=%s, got:\n%s", version, output)
			}
		})
	}
}

// Outside a checkout - and in a shallow or tagless clone, where `git
// describe` can only report a commit - the derived value is normalized to
// the `dev` sentinel internal/update already understands, never to a label
// that looks like a version and is not.
func TestMakeBuildFallsBackToDevWhenGitDescribeCarriesNoVersion(t *testing.T) {
	skipMakeBuildTestsOnWindows(t)

	makePath := lookupMake(t)
	workDir := writeTestMakeWorkspace(t)

	output := runMakeDryBuild(t, makePath, workDir, nil)

	if !strings.Contains(output, "/internal/buildinfo.Version=dev") {
		t.Fatalf("make build outside a checkout should stamp the dev sentinel, got:\n%s", output)
	}
}

// The release path: a checkout sitting exactly at a release tag is the
// release build and stamps that tag verbatim, exactly as before fork
// stamping existed.
func TestMakeBuildDerivesTheDescribedVersionInATaggedCheckout(t *testing.T) {
	skipMakeBuildTestsOnWindows(t)

	makePath := lookupMake(t)
	gitPath, err := testgit.RealGit()
	if err != nil {
		t.Skip("real git not available")
	}

	workDir := writeTestMakeWorkspace(t)
	commitTestMakeWorkspace(t, gitPath, workDir)
	runScratchGit(t, gitPath, workDir, "tag", "v1.76.0")

	output := runMakeDryBuild(t, makePath, workDir, nil)

	if !strings.Contains(output, "/internal/buildinfo.Version=v1.76.0 ") {
		t.Fatalf("make build should stamp the described version, got:\n%s", output)
	}
}

// The default every from-source build actually takes: away from a release
// tag, the build stamps the fork stamp - the newest release tag the clone
// knows (the upstream semantic version), the `-fork-` marker, and the
// build's own short commit. This is the string the bootstrap version probe
// reads to prove the minimum-version floor while identifying the binary as
// this fork's build rather than stock upstream.
func TestMakeBuildStampsTheForkStampByDefaultOffATag(t *testing.T) {
	skipMakeBuildTestsOnWindows(t)

	makePath := lookupMake(t)
	gitPath, err := testgit.RealGit()
	if err != nil {
		t.Skip("real git not available")
	}

	workDir := writeTestMakeWorkspace(t)
	commitTestMakeWorkspace(t, gitPath, workDir)
	runScratchGit(t, gitPath, workDir, "tag", "v1.83.1")
	runScratchGit(t, gitPath, workDir, "commit", "-q", "--allow-empty", "-m", "after the tag")

	revCmd := exec.Command(gitPath, "rev-parse", "--short", "HEAD")
	revCmd.Dir = workDir
	revCmd.Env = scratchEnv(t)
	revOut, err := revCmd.Output()
	if err != nil {
		t.Fatalf("rev-parse --short HEAD failed: %v", err)
	}
	short := strings.TrimSpace(string(revOut))

	output := runMakeDryBuild(t, makePath, workDir, nil)

	want := "/internal/buildinfo.Version=1.83.1-fork-" + short + " "
	if !strings.Contains(output, want) {
		t.Fatalf("make build should stamp %q by default off a release tag, got:\n%s", strings.TrimSpace(want), output)
	}
}

// A checkout sitting exactly at a non-version tag is not a release build:
// this repository itself carries tags like `channels` whose name carries no
// version number, and stamping one verbatim would ship a binary every
// minimum-version check reads as not installed - inviting an install over
// the very binary running - while bypassing the hand-passed refusal that
// exists to stop exactly that. The exact tag is taken verbatim only when it
// is a comparable version; otherwise the build takes the fork stamp.
func TestMakeBuildDoesNotStampANonVersionExactTagVerbatim(t *testing.T) {
	skipMakeBuildTestsOnWindows(t)

	makePath := lookupMake(t)
	gitPath, err := testgit.RealGit()
	if err != nil {
		t.Skip("real git not available")
	}

	workDir := writeTestMakeWorkspace(t)
	commitTestMakeWorkspace(t, gitPath, workDir)
	runScratchGit(t, gitPath, workDir, "tag", "channels")

	output := runMakeDryBuild(t, makePath, workDir, nil)

	if strings.Contains(output, "/internal/buildinfo.Version=channels") {
		t.Fatalf("make build must not stamp a non-version exact tag verbatim, got:\n%s", output)
	}
	// No release tag exists in this clone either, so the fallback is the
	// dev sentinel, not a label shaped like a version.
	if !strings.Contains(output, "/internal/buildinfo.Version=dev") {
		t.Fatalf("make build at a non-version exact tag should fall back to the dev sentinel, got:\n%s", output)
	}
}

// The newest release the clone knows is ranked by the semver comparison
// internal/update uses: a prerelease never outranks the release that
// superseded it, no matter how git or coreutils would sort the refnames.
// With both v1.84.0-rc.1 and v1.84.0 present, `git tag --sort=-v:refname`
// and coreutils `sort -V` both hand back the rc first, so a build off a tag
// used to stamp a version the clone's own release had superseded.
func TestMakeBuildStampsTheReleaseAboveItsPrerelease(t *testing.T) {
	skipMakeBuildTestsOnWindows(t)

	makePath := lookupMake(t)
	gitPath, err := testgit.RealGit()
	if err != nil {
		t.Skip("real git not available")
	}

	workDir := writeTestMakeWorkspace(t)
	commitTestMakeWorkspace(t, gitPath, workDir)
	runScratchGit(t, gitPath, workDir, "tag", "v1.84.0-rc.1")
	runScratchGit(t, gitPath, workDir, "commit", "-q", "--allow-empty", "-m", "the release")
	runScratchGit(t, gitPath, workDir, "tag", "v1.84.0")
	runScratchTagOnNewCommit(t, gitPath, workDir)

	output := runMakeDryBuild(t, makePath, workDir, nil)

	if !strings.Contains(output, "/internal/buildinfo.Version=1.84.0-fork-") {
		t.Fatalf("make build should stamp the release a prerelease never outranks, got:\n%s", output)
	}
	if strings.Contains(output, "buildinfo.Version=1.84.0-rc.") {
		t.Fatalf("make build must not stamp a prerelease its own release supersedes, got:\n%s", output)
	}
}

// Semver ranks prerelease identifiers numerically (`rc.10` above `rc.2`),
// so among prereleases of one core the ranking must follow semver too, not
// lexicographic text order.
func TestMakeBuildRanksPrereleaseIdentifiersNumerically(t *testing.T) {
	skipMakeBuildTestsOnWindows(t)

	makePath := lookupMake(t)
	gitPath, err := testgit.RealGit()
	if err != nil {
		t.Skip("real git not available")
	}

	workDir := writeTestMakeWorkspace(t)
	commitTestMakeWorkspace(t, gitPath, workDir)
	runScratchGit(t, gitPath, workDir, "tag", "v1.84.0-rc.2")
	runScratchGit(t, gitPath, workDir, "tag", "v1.84.0-rc.10")
	runScratchTagOnNewCommit(t, gitPath, workDir)

	output := runMakeDryBuild(t, makePath, workDir, nil)

	if !strings.Contains(output, "/internal/buildinfo.Version=1.84.0-rc.10-fork-") {
		t.Fatalf("make build should rank rc.10 above rc.2 the way semver does, got:\n%s", output)
	}
}

// Semver ranks a numeric prerelease identifier below an alphanumeric one:
// with both v1.86.0-2 and v1.86.0-beta known, the beta is the newer
// prerelease and an off-tag build must stamp it, not the numeric one git's
// refname sort would hand back.
func TestMakeBuildRanksNumericPrereleaseIdentifiersBelowAlphanumeric(t *testing.T) {
	skipMakeBuildTestsOnWindows(t)

	makePath := lookupMake(t)
	gitPath, err := testgit.RealGit()
	if err != nil {
		t.Skip("real git not available")
	}

	workDir := writeTestMakeWorkspace(t)
	commitTestMakeWorkspace(t, gitPath, workDir)
	runScratchGit(t, gitPath, workDir, "tag", "v1.86.0-2")
	runScratchGit(t, gitPath, workDir, "tag", "v1.86.0-beta")
	runScratchTagOnNewCommit(t, gitPath, workDir)

	output := runMakeDryBuild(t, makePath, workDir, nil)

	if !strings.Contains(output, "/internal/buildinfo.Version=1.86.0-beta-fork-") {
		t.Fatalf("make build should stamp the alphanumeric prerelease semver ranks above the numeric one, got:\n%s", output)
	}
	if strings.Contains(output, "buildinfo.Version=1.86.0-2-fork-") {
		t.Fatalf("make build must not stamp the numeric prerelease semver ranks below an alphanumeric one, got:\n%s", output)
	}
}

// Semver orders alphanumeric prerelease identifiers of one core by text
// order: `rc.1` is newer than `alpha.1`, so a clone knowing both stamps the
// rc, never the alpha.
func TestMakeBuildRanksPrereleaseIdentifiersByTextOrder(t *testing.T) {
	skipMakeBuildTestsOnWindows(t)

	makePath := lookupMake(t)
	gitPath, err := testgit.RealGit()
	if err != nil {
		t.Skip("real git not available")
	}

	workDir := writeTestMakeWorkspace(t)
	commitTestMakeWorkspace(t, gitPath, workDir)
	runScratchGit(t, gitPath, workDir, "tag", "v1.86.0-rc.1")
	runScratchGit(t, gitPath, workDir, "tag", "v1.86.0-alpha.1")
	runScratchTagOnNewCommit(t, gitPath, workDir)

	output := runMakeDryBuild(t, makePath, workDir, nil)

	if !strings.Contains(output, "/internal/buildinfo.Version=1.86.0-rc.1-fork-") {
		t.Fatalf("make build should stamp rc.1, which semver ranks above alpha.1, got:\n%s", output)
	}
	if strings.Contains(output, "buildinfo.Version=1.86.0-alpha.1-fork-") {
		t.Fatalf("make build must not stamp alpha.1 below the rc semver ranks above it, got:\n%s", output)
	}
}

// A prerelease is everything after the version's first hyphen, so an
// identifier may itself carry hyphens: `1.84.0-foo-bar`'s single identifier
// `foo-bar` outranks `1.84.0-foo.bar`'s `foo`, which is only a prefix of
// it, and the build must stamp it.
func TestMakeBuildReadsHyphenBearingPrereleaseIdentifiers(t *testing.T) {
	skipMakeBuildTestsOnWindows(t)

	makePath := lookupMake(t)
	gitPath, err := testgit.RealGit()
	if err != nil {
		t.Skip("real git not available")
	}

	workDir := writeTestMakeWorkspace(t)
	commitTestMakeWorkspace(t, gitPath, workDir)
	runScratchGit(t, gitPath, workDir, "tag", "v1.84.0-foo-bar")
	runScratchGit(t, gitPath, workDir, "tag", "v1.84.0-foo.bar")
	runScratchTagOnNewCommit(t, gitPath, workDir)

	output := runMakeDryBuild(t, makePath, workDir, nil)

	if !strings.Contains(output, "/internal/buildinfo.Version=1.84.0-foo-bar-fork-") {
		t.Fatalf("make build should stamp the hyphen-bearing prerelease whose identifier outranks its prefix, got:\n%s", output)
	}
	if strings.Contains(output, "buildinfo.Version=1.84.0-foo.bar-fork-") {
		t.Fatalf("make build must not stamp the dot-split prerelease semver ranks below, got:\n%s", output)
	}
}

// The tag scan accepts the optional v prefix exactly as the guard's own
// version pattern does, and ranks by the stripped core: a clone whose only
// release tag is v2.0.0 - or the same tag without the v - stamps
// 2.0.0-fork-<sha>, never the silent dev fallback.
func TestMakeBuildNamesTheOnlyReleaseTagWhateverShapeItHas(t *testing.T) {
	skipMakeBuildTestsOnWindows(t)

	makePath := lookupMake(t)
	gitPath, err := testgit.RealGit()
	if err != nil {
		t.Skip("real git not available")
	}

	for _, tag := range []string{"v2.0.0", "2.0.0"} {
		t.Run(tag, func(t *testing.T) {
			workDir := writeTestMakeWorkspace(t)
			commitTestMakeWorkspace(t, gitPath, workDir)
			// The tag lands on a side branch so neither `git describe`
			// nor reachability can supply the release and mask what the
			// scan itself accepts.
			runScratchGit(t, gitPath, workDir, "checkout", "-q", "-b", "side")
			runScratchGit(t, gitPath, workDir, "commit", "-q", "--allow-empty", "-m", "the release")
			runScratchGit(t, gitPath, workDir, "tag", tag)
			runScratchGit(t, gitPath, workDir, "checkout", "-q", "-")

			revCmd := exec.Command(gitPath, "rev-parse", "--short", "HEAD")
			revCmd.Dir = workDir
			revCmd.Env = scratchEnv(t)
			revOut, err := revCmd.Output()
			if err != nil {
				t.Fatalf("rev-parse --short HEAD failed: %v", err)
			}
			short := strings.TrimSpace(string(revOut))

			output := runMakeDryBuild(t, makePath, workDir, nil)

			want := "/internal/buildinfo.Version=2.0.0-fork-" + short + " "
			if !strings.Contains(output, want) {
				t.Fatalf("make build should stamp %q from the only release tag, got:\n%s", strings.TrimSpace(want), output)
			}
		})
	}
}

// Only tags the version pattern accepts may enter the release ranking: a
// version-shaped junk tag like `v1.84fix` matches the scan's git glob but is
// not a comparable version, and ranking it above the valid release the clone
// knows hands the stamp a core no floor check can read - silently
// downgrading the build to the `dev` sentinel while a valid release is
// present. The junk tag must be ignored and the known release stamped.
func TestMakeBuildIgnoresTagsTheVersionPatternRejects(t *testing.T) {
	skipMakeBuildTestsOnWindows(t)

	makePath := lookupMake(t)
	gitPath, err := testgit.RealGit()
	if err != nil {
		t.Skip("real git not available")
	}

	workDir := writeTestMakeWorkspace(t)
	commitTestMakeWorkspace(t, gitPath, workDir)
	runScratchGit(t, gitPath, workDir, "tag", "v1.83.1")
	// The junk tag lands on a side branch, unreachable from HEAD, and
	// outranks the valid release under the scan's own ordering, so nothing
	// but acceptance-by-pattern keeps it from shadowing the release.
	runScratchGit(t, gitPath, workDir, "checkout", "-q", "-b", "side")
	runScratchGit(t, gitPath, workDir, "commit", "-q", "--allow-empty", "-m", "side")
	runScratchGit(t, gitPath, workDir, "tag", "v1.84fix")
	runScratchGit(t, gitPath, workDir, "checkout", "-q", "-")
	runScratchTagOnNewCommit(t, gitPath, workDir)

	revCmd := exec.Command(gitPath, "rev-parse", "--short", "HEAD")
	revCmd.Dir = workDir
	revCmd.Env = scratchEnv(t)
	revOut, err := revCmd.Output()
	if err != nil {
		t.Fatalf("rev-parse --short HEAD failed: %v", err)
	}
	short := strings.TrimSpace(string(revOut))

	output := runMakeDryBuild(t, makePath, workDir, nil)

	want := "/internal/buildinfo.Version=1.83.1-fork-" + short + " "
	if !strings.Contains(output, want) {
		t.Fatalf("make build should stamp %q despite a junk version-shaped tag, got:\n%s", strings.TrimSpace(want), output)
	}
	if strings.Contains(output, "/internal/buildinfo.Version=dev") {
		t.Fatalf("a tag the version pattern rejects must not shadow the known release into the dev sentinel, got:\n%s", output)
	}
}

// A clone holding only a prerelease tag still knows a release: the fork
// stamp names that prerelease, never a fabricated stable version.
func TestMakeBuildUsesAPrereleaseOnlyClonesHighestPrerelease(t *testing.T) {
	skipMakeBuildTestsOnWindows(t)

	makePath := lookupMake(t)
	gitPath, err := testgit.RealGit()
	if err != nil {
		t.Skip("real git not available")
	}

	workDir := writeTestMakeWorkspace(t)
	commitTestMakeWorkspace(t, gitPath, workDir)
	runScratchGit(t, gitPath, workDir, "tag", "v1.84.0-rc.1")
	runScratchTagOnNewCommit(t, gitPath, workDir)

	output := runMakeDryBuild(t, makePath, workDir, nil)

	if !strings.Contains(output, "/internal/buildinfo.Version=1.84.0-rc.1-fork-") {
		t.Fatalf("a clone that knows only a prerelease should stamp that prerelease, got:\n%s", output)
	}
}

// With a real release known to the clone, a non-version exact tag takes the
// fork stamp of that release rather than its own name: the release path is
// reserved for comparable version tags, everything else identifies as this
// fork's build.
func TestMakeBuildForkStampsWhenANonVersionTagSitsAtHead(t *testing.T) {
	skipMakeBuildTestsOnWindows(t)

	makePath := lookupMake(t)
	gitPath, err := testgit.RealGit()
	if err != nil {
		t.Skip("real git not available")
	}

	workDir := writeTestMakeWorkspace(t)
	commitTestMakeWorkspace(t, gitPath, workDir)
	runScratchGit(t, gitPath, workDir, "tag", "v1.83.1")
	runScratchGit(t, gitPath, workDir, "commit", "-q", "--allow-empty", "-m", "after the tag")
	runScratchGit(t, gitPath, workDir, "tag", "channels")

	revCmd := exec.Command(gitPath, "rev-parse", "--short", "HEAD")
	revCmd.Dir = workDir
	revCmd.Env = scratchEnv(t)
	revOut, err := revCmd.Output()
	if err != nil {
		t.Fatalf("rev-parse --short HEAD failed: %v", err)
	}
	short := strings.TrimSpace(string(revOut))

	output := runMakeDryBuild(t, makePath, workDir, nil)

	want := "/internal/buildinfo.Version=1.83.1-fork-" + short + " "
	if !strings.Contains(output, want) {
		t.Fatalf("make build should stamp %q when a non-version tag sits at head, got:\n%s", strings.TrimSpace(want), output)
	}
	if strings.Contains(output, "/internal/buildinfo.Version=channels") {
		t.Fatalf("make build must not stamp a non-version exact tag verbatim, got:\n%s", output)
	}
}

// A tagless clone knows no release tag, so the fork stamp's core falls back
// to the 0.0.0 placeholder and the suggestion must be a value the same guard
// accepts, never a bare commit dressed up with a marker.
func TestMakeBuildSuggestsAVersionItsOwnGuardAcceptsInATaglessCheckout(t *testing.T) {
	skipMakeBuildTestsOnWindows(t)

	makePath := lookupMake(t)
	gitPath, err := testgit.RealGit()
	if err != nil {
		t.Skip("real git not available")
	}

	workDir := writeTestMakeWorkspace(t)
	commitTestMakeWorkspace(t, gitPath, workDir)

	output := runMakeBuildExpectingRefusal(t, makePath, workDir, []string{"build", "VERSION=fork-x"}, nil)

	suggested := suggestedVersion(t, output)
	if !strings.HasPrefix(suggested, "0.0.0-fork-") {
		t.Fatalf("a tagless checkout knows no release, so the suggestion should carry the placeholder core rather than a plausible release number, got %q in:\n%s", suggested, output)
	}
	if !strings.Contains(output, "placeholder") {
		t.Fatalf("the refusal should say the placeholder core must be replaced, got:\n%s", output)
	}
	assertForkStamp(t, suggested)

	accepted := runMakeDryBuild(t, makePath, workDir, map[string]string{"VERSION": suggested})
	if !strings.Contains(accepted, "/internal/buildinfo.Version="+suggested) {
		t.Fatalf("the suggested VERSION=%s should itself pass the guard, got:\n%s", suggested, accepted)
	}
}

// The suggestion a refusal prints is the fork stamp itself: the newest
// release the repository knows plus the fork marker plus the commit, the
// exact shape the default stamps when nothing is passed by hand.
func TestMakeBuildSuggestsTheKnownReleaseAsAForkStamp(t *testing.T) {
	skipMakeBuildTestsOnWindows(t)

	makePath := lookupMake(t)
	gitPath, err := testgit.RealGit()
	if err != nil {
		t.Skip("real git not available")
	}

	workDir := writeTestMakeWorkspace(t)
	commitTestMakeWorkspace(t, gitPath, workDir)
	runScratchGit(t, gitPath, workDir, "tag", "v1.76.0")
	runScratchGit(t, gitPath, workDir, "commit", "-q", "--allow-empty", "-m", "after the tag")

	output := runMakeBuildExpectingRefusal(t, makePath, workDir, []string{"build", "VERSION=fork-x"}, nil)

	suggested := suggestedVersion(t, output)
	if !strings.HasPrefix(suggested, "1.76.0-fork-") {
		t.Fatalf("the suggestion should carry the release the repository knows plus the fork stamp, got %q in:\n%s", suggested, output)
	}
	if strings.Contains(output, "placeholder") {
		t.Fatalf("a described release is not a placeholder, got:\n%s", output)
	}
	assertForkStamp(t, suggested)

	accepted := runMakeDryBuild(t, makePath, workDir, map[string]string{"VERSION": suggested})
	if !strings.Contains(accepted, "/internal/buildinfo.Version="+suggested) {
		t.Fatalf("the suggested VERSION=%s should itself pass the guard, got:\n%s", suggested, accepted)
	}
}

// The refusal belongs to the targets that stamp a binary: an ambient VERSION
// nobody aimed at this build must not kill a target that ships nothing.
func TestMakeCleanSurvivesAnUncomparableVersionInTheEnvironment(t *testing.T) {
	skipMakeBuildTestsOnWindows(t)

	makePath := lookupMake(t)
	workDir := writeTestMakeWorkspace(t)

	runMakeTarget(t, makePath, workDir, []string{"clean"}, map[string]string{"VERSION": "fork-7e84d0d"})
}

// A bare `make` with no goal named builds the binary, so it carries the
// guard like any other stamping invocation.
func TestMakeWithNoGoalBuildsAndCarriesTheGuard(t *testing.T) {
	skipMakeBuildTestsOnWindows(t)

	makePath := lookupMake(t)
	workDir := writeTestMakeWorkspace(t)

	built := runMakeTarget(t, makePath, workDir, []string{"-n"}, map[string]string{"VERSION": "v1.76.0+fork.7e84d0d"})
	if !strings.Contains(built, "/internal/buildinfo.Version=v1.76.0+fork.7e84d0d") {
		t.Fatalf("a bare make should stamp and build the binary, got:\n%s", built)
	}
	if !strings.Contains(built, "-o bin/no-mistakes") {
		t.Fatalf("a bare make should build bin/no-mistakes, got:\n%s", built)
	}

	refused := runMakeBuildExpectingRefusal(t, makePath, workDir, nil, map[string]string{"VERSION": "fork-7e84d0d"})
	if !strings.Contains(refused, "carries no comparable version number") {
		t.Fatalf("a bare make should refuse an uncomparable VERSION, got:\n%s", refused)
	}
}

// A fork of a prerelease must be told to call itself that prerelease, not
// the release above it that was never published.
func TestMakeBuildSuggestionKeepsAPrereleaseTagsIdentifier(t *testing.T) {
	skipMakeBuildTestsOnWindows(t)

	makePath := lookupMake(t)
	gitPath, err := testgit.RealGit()
	if err != nil {
		t.Skip("real git not available")
	}

	workDir := writeTestMakeWorkspace(t)
	commitTestMakeWorkspace(t, gitPath, workDir)
	runScratchGit(t, gitPath, workDir, "tag", "v1.77.0-rc.1")
	runScratchGit(t, gitPath, workDir, "commit", "-q", "--allow-empty", "-m", "after the prerelease")

	output := runMakeBuildExpectingRefusal(t, makePath, workDir, []string{"build", "VERSION=fork-x"}, nil)

	suggested := suggestedVersion(t, output)
	if !strings.HasPrefix(suggested, "1.77.0-rc.1-fork-") {
		t.Fatalf("the suggestion should keep the described prerelease rather than name the unreleased release above it, got %q in:\n%s", suggested, output)
	}

	accepted := runMakeDryBuild(t, makePath, workDir, map[string]string{"VERSION": suggested})
	if !strings.Contains(accepted, "/internal/buildinfo.Version="+suggested) {
		t.Fatalf("the suggested VERSION=%s should itself pass the guard, got:\n%s", suggested, accepted)
	}
}

var suggestionPattern = regexp.MustCompile(`Pass a comparable version such as VERSION=(\S+?),`)

func suggestedVersion(t *testing.T, refusal string) string {
	t.Helper()

	match := suggestionPattern.FindStringSubmatch(refusal)
	if match == nil {
		t.Fatalf("refusal should suggest a replacement version, got:\n%s", refusal)
	}
	return match[1]
}

// assertForkStamp checks the fork stamp shape a suggestion (and the default)
// carries: an upstream semantic version core, the `-fork-` marker, and the
// build's short commit - e.g. `1.83.1-fork-6911e03` - which is what every
// minimum-version probe parses back as a comparable version identifying a
// fork build.
func assertForkStamp(t *testing.T, version string) {
	t.Helper()

	core, commit, found := strings.Cut(version, "-fork-")
	if !found {
		t.Fatalf("suggested VERSION=%s should carry the -fork- marker", version)
	}
	if core == "" || commit == "" {
		t.Fatalf("suggested VERSION=%s should keep a release core in front of the marker and a commit after it", version)
	}
	if strings.HasPrefix(core, "v") {
		t.Fatalf("suggested VERSION=%s should carry the plain upstream semantic version, not a v-prefixed one", version)
	}
}

func commitTestMakeWorkspace(t *testing.T, gitPath, workDir string) {
	t.Helper()
	runScratchGit(t, gitPath, workDir, "init", "-q")
	runScratchGit(t, gitPath, workDir, "add", "Makefile")
	runScratchGit(t, gitPath, workDir, "commit", "-q", "-m", "makefile")
}

// runScratchTagOnNewCommit moves HEAD to a fresh empty commit so a test can
// assert what an off-tag build stamps without disturbing tags already
// placed on earlier commits.
func runScratchTagOnNewCommit(t *testing.T, gitPath, workDir string) {
	t.Helper()
	runScratchGit(t, gitPath, workDir, "commit", "-q", "--allow-empty", "-m", "after the tagged commits")
}

// Make itself runs `git describe` from the scratch workspace, so the
// measuring invocations need the same repository-discovery and config
// isolation as the setup commands: an inherited GIT_DIR would otherwise
// point describe at the real repository and steer the value under test.
func scratchEnv(t *testing.T, alsoExcluded ...string) []string {
	t.Helper()

	excluded := append([]string{"GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_CONFIG_COUNT", "GIT_CONFIG_GLOBAL"}, alsoExcluded...)
	return append(filteredEnv(os.Environ(), excluded...),
		"GIT_CONFIG_GLOBAL="+filepath.Join(t.TempDir(), "gitconfig"),
		"GIT_CONFIG_NOSYSTEM=1",
	)
}

// The developer's own git config must not decide this: commit signing or an
// inherited GIT_DIR would otherwise fail the commit and report correct
// Makefile behaviour as a red.
func runScratchGit(t *testing.T, gitPath, workDir string, args ...string) {
	t.Helper()

	cmd := exec.Command(gitPath, args...)
	cmd.Dir = workDir
	cmd.Env = append(scratchEnv(t),
		"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.com",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v failed: %v\n%s", args, err, out)
	}
}

func lookupMake(t *testing.T) string {
	t.Helper()
	makePath, err := exec.LookPath("make")
	if err != nil {
		t.Skip("make not available")
	}
	return makePath
}

func skipMakeBuildTestsOnWindows(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("make build tests are POSIX-oriented")
	}
}

func writeTestMakeWorkspace(t *testing.T) string {
	t.Helper()

	data, err := os.ReadFile("Makefile")
	if err != nil {
		t.Fatal(err)
	}
	workDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(workDir, "Makefile"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	return workDir
}

func runMakeTarget(t *testing.T, makePath, workDir string, makeArgs []string, extraEnv map[string]string) string {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, makePath, makeArgs...)
	cmd.Dir = workDir
	cmd.Env = scratchEnv(t, "VERSION", "UMAMI_HOST", "UMAMI_WEBSITE_ID", "NO_MISTAKES_UMAMI_HOST", "NO_MISTAKES_UMAMI_WEBSITE_ID")
	for key, value := range extraEnv {
		cmd.Env = append(cmd.Env, key+"="+value)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("make %v failed: %v\n%s", makeArgs, err, out)
	}
	return string(out)
}

func runMakeDryBuild(t *testing.T, makePath, workDir string, extraEnv map[string]string) string {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, makePath, "-n", "build")
	cmd.Dir = workDir
	cmd.Env = scratchEnv(t, "VERSION", "UMAMI_HOST", "UMAMI_WEBSITE_ID", "NO_MISTAKES_UMAMI_HOST", "NO_MISTAKES_UMAMI_WEBSITE_ID")
	for key, value := range extraEnv {
		cmd.Env = append(cmd.Env, key+"="+value)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("make -n build failed: %v\n%s", err, out)
	}
	return string(out)
}

// The refusal is asserted on a real invocation: `make -n` only echoes the
// guard's recipe line, where a quoting defect in it stays inert text, so a
// dry run can pass a refusal that dies as a shell syntax error and never
// prints its guidance. A real build exercises the recipe the way an
// operator hits it.
func runMakeBuildExpectingRefusal(t *testing.T, makePath, workDir string, makeArgs []string, extraEnv map[string]string) string {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if len(makeArgs) == 0 {
		makeArgs = []string{"build"}
	}
	cmd := exec.CommandContext(ctx, makePath, makeArgs...)
	cmd.Dir = workDir
	cmd.Env = scratchEnv(t, "VERSION", "UMAMI_HOST", "UMAMI_WEBSITE_ID", "NO_MISTAKES_UMAMI_HOST", "NO_MISTAKES_UMAMI_WEBSITE_ID")
	for key, value := range extraEnv {
		cmd.Env = append(cmd.Env, key+"="+value)
	}
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("make %v should have refused, got:\n%s", makeArgs, out)
	}
	return string(out)
}
