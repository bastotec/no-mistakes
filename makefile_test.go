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
	for _, version := range []string{"fork-7e84d0d", "7e84d0d", "dev", "fork", "1.2.3.4", "v2026.01.15.3", "1.2.3-rc.", "1.2.3-rc..1"} {
		t.Run(version, func(t *testing.T) {
			output := runMakeDryBuildExpectingFailure(t, makePath, workDir, []string{"build", "VERSION=" + version}, nil)

			if !strings.Contains(output, "carries no comparable version number") {
				t.Fatalf("make build should refuse VERSION=%s with an explanation, got:\n%s", version, output)
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

	output := runMakeDryBuildExpectingFailure(t, makePath, workDir, []string{"build"}, map[string]string{"VERSION": "fork-7e84d0d"})

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

// The documented from-source path: a full clone already knows a comparable
// version, which is the one the broken build threw away.
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

// A tagless clone derives a bare commit from `git describe`, which the guard
// itself refuses. The suggested replacement must therefore be a value the
// same guard accepts, not that commit dressed up with `+fork`.
func TestMakeBuildSuggestsAVersionItsOwnGuardAcceptsInATaglessCheckout(t *testing.T) {
	skipMakeBuildTestsOnWindows(t)

	makePath := lookupMake(t)
	gitPath, err := testgit.RealGit()
	if err != nil {
		t.Skip("real git not available")
	}

	workDir := writeTestMakeWorkspace(t)
	commitTestMakeWorkspace(t, gitPath, workDir)

	output := runMakeDryBuildExpectingFailure(t, makePath, workDir, []string{"build", "VERSION=fork-x"}, nil)

	suggested := suggestedVersion(t, output)
	if !strings.HasPrefix(suggested, "v1.2.3+") {
		t.Fatalf("a tagless checkout knows no release, so the suggestion should fall back to the placeholder core, got %q in:\n%s", suggested, output)
	}
	assertPureBuildMetadata(t, suggested)

	accepted := runMakeDryBuild(t, makePath, workDir, map[string]string{"VERSION": suggested})
	if !strings.Contains(accepted, "/internal/buildinfo.Version="+suggested) {
		t.Fatalf("the suggested VERSION=%s should itself pass the guard, got:\n%s", suggested, accepted)
	}
}

// A prerelease-shaped fork marker sorts BELOW the release it names, so the
// updater offers to replace the fork with the older build it came from. The
// suggestion must carry the release the repository knows plus the commit as
// build metadata instead.
func TestMakeBuildSuggestsTheKnownReleaseWithBuildMetadataNotAPrerelease(t *testing.T) {
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

	output := runMakeDryBuildExpectingFailure(t, makePath, workDir, []string{"build", "VERSION=fork-x"}, nil)

	suggested := suggestedVersion(t, output)
	if !strings.HasPrefix(suggested, "v1.76.0+") {
		t.Fatalf("the suggestion should carry the release the repository knows, got %q in:\n%s", suggested, output)
	}
	assertPureBuildMetadata(t, suggested)

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

// A bare `make` with no goal named still builds, so it still refuses.
func TestMakeWithNoGoalRefusesAnUncomparableVersion(t *testing.T) {
	skipMakeBuildTestsOnWindows(t)

	makePath := lookupMake(t)
	workDir := writeTestMakeWorkspace(t)

	output := runMakeDryBuildExpectingFailure(t, makePath, workDir, nil, map[string]string{"VERSION": "fork-7e84d0d"})

	if !strings.Contains(output, "carries no comparable version number") {
		t.Fatalf("a bare make should refuse an uncomparable VERSION, got:\n%s", output)
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

func assertPureBuildMetadata(t *testing.T, version string) {
	t.Helper()

	core, metadata, found := strings.Cut(version, "+")
	if !found {
		t.Fatalf("suggested VERSION=%s should carry the fork marker as build metadata", version)
	}
	if strings.Contains(core, "-") {
		t.Fatalf("suggested VERSION=%s is prerelease-shaped, which semver ranks below the release it names", version)
	}
	if !strings.HasPrefix(metadata, "fork") {
		t.Fatalf("suggested VERSION=%s should mark the build as a fork, got metadata %q", version, metadata)
	}
}

func commitTestMakeWorkspace(t *testing.T, gitPath, workDir string) {
	t.Helper()
	runScratchGit(t, gitPath, workDir, "init", "-q")
	runScratchGit(t, gitPath, workDir, "add", "Makefile")
	runScratchGit(t, gitPath, workDir, "commit", "-q", "-m", "makefile")
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

func runMakeDryBuildExpectingFailure(t *testing.T, makePath, workDir string, makeArgs []string, extraEnv map[string]string) string {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	args := append([]string{"-n"}, makeArgs...)
	cmd := exec.CommandContext(ctx, makePath, args...)
	cmd.Dir = workDir
	cmd.Env = scratchEnv(t, "VERSION", "UMAMI_HOST", "UMAMI_WEBSITE_ID", "NO_MISTAKES_UMAMI_HOST", "NO_MISTAKES_UMAMI_WEBSITE_ID")
	for key, value := range extraEnv {
		cmd.Env = append(cmd.Env, key+"="+value)
	}
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("make -n %v should have failed, got:\n%s", makeArgs, out)
	}
	return string(out)
}
