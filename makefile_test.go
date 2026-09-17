package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
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

	for _, version := range []string{"fork-7e84d0d", "7e84d0d", "dev", "fork"} {
		t.Run(version, func(t *testing.T) {
			output := runMakeDryBuildExpectingFailure(t, makePath, workDir, []string{"VERSION=" + version}, nil)

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

	output := runMakeDryBuildExpectingFailure(t, makePath, workDir, nil, map[string]string{"VERSION": "fork-7e84d0d"})

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

	for _, version := range []string{"v1.76.0", "1.76.0", "v1.76.0-5-g7e84d0d", "v1.76.0-5-g7e84d0d-dirty+fork", "v1.76.0+fork.7e84d0d"} {
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
	runGit := func(args ...string) {
		t.Helper()
		cmd := exec.Command(gitPath, args...)
		cmd.Dir = workDir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.com",
			"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.com",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v failed: %v\n%s", args, err, out)
		}
	}
	runGit("init", "-q")
	runGit("add", "Makefile")
	runGit("commit", "-q", "-m", "makefile")
	runGit("tag", "v1.76.0")

	output := runMakeDryBuild(t, makePath, workDir, nil)

	if !strings.Contains(output, "/internal/buildinfo.Version=v1.76.0") {
		t.Fatalf("make build should stamp the described version, got:\n%s", output)
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

func runMakeDryBuild(t *testing.T, makePath, workDir string, extraEnv map[string]string) string {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, makePath, "-n", "build")
	cmd.Dir = workDir
	cmd.Env = filteredEnv(os.Environ(), "VERSION", "UMAMI_HOST", "UMAMI_WEBSITE_ID", "NO_MISTAKES_UMAMI_HOST", "NO_MISTAKES_UMAMI_WEBSITE_ID")
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

	args := append([]string{"-n", "build"}, makeArgs...)
	cmd := exec.CommandContext(ctx, makePath, args...)
	cmd.Dir = workDir
	cmd.Env = filteredEnv(os.Environ(), "VERSION", "UMAMI_HOST", "UMAMI_WEBSITE_ID", "NO_MISTAKES_UMAMI_HOST", "NO_MISTAKES_UMAMI_WEBSITE_ID")
	for key, value := range extraEnv {
		cmd.Env = append(cmd.Env, key+"="+value)
	}
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("make -n build should have failed, got:\n%s", out)
	}
	return string(out)
}
