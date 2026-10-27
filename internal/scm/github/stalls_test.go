package github

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/scm"
)

func TestClassifyStalledJob(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		name string
		job  stallJob
		want string
	}{
		{"queued-no-runner", stallJob{Status: "queued", CreatedAt: now.Add(-11 * time.Minute)}, "queued-no-runner"},
		{"normal-queue", stallJob{Status: "queued", CreatedAt: now.Add(-time.Minute)}, ""},
		{"runner-assigned", stallJob{Status: "queued", RunnerID: 1, CreatedAt: now.Add(-time.Hour)}, ""},
		{"unknown-time", stallJob{Status: "queued"}, ""},
		{"running-past-timeout", stallJob{Status: "in_progress", StartedAt: now.Add(-31 * time.Minute)}, "running-past-timeout"},
		{"running-in-budget", stallJob{Status: "in_progress", StartedAt: now.Add(-time.Minute)}, ""},
		{"environment-wait", stallJob{Status: "waiting", CreatedAt: now.Add(-time.Hour)}, ""},
		{"concurrency-wait", stallJob{Status: "pending", CreatedAt: now.Add(-time.Hour)}, ""},
		{"provider-timeout", stallJob{Status: "completed", Conclusion: "timed_out"}, "running-past-timeout"},
		{"real-failure", stallJob{Status: "completed", Conclusion: "failure"}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyStalledJob(tc.job, 30*time.Minute, now); got != tc.want {
				t.Fatalf("got %q want %q", got, tc.want)
			}
		})
	}
}

func TestCIStallsProviderAnnotationsAndPinnedWorkflow(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	for _, class := range []string{"queued-no-runner", "provider-no-start", "running-past-timeout"} {
		t.Run(class, func(t *testing.T) {
			job := stallJob{ID: 2, Name: "build", Status: "queued", CreatedAt: now.Add(-11 * time.Minute), CheckRunURL: "https://api.github.com/repos/test/repo/check-runs/3", HTMLURL: "https://github.com/test/repo/actions/runs/1/job/2"}
			if class == "provider-no-start" {
				job.Status = "completed"
				job.Conclusion = "failure"
			}
			if class == "running-past-timeout" {
				job.Status = "in_progress"
				job.RunnerID = 4
				job.StartedAt = now.Add(-31 * time.Minute)
			}
			jobs, _ := json.Marshal(map[string]any{"jobs": []stallJob{job}})
			annotations := "[]"
			if class == "provider-no-start" {
				annotations = `[{"message":"The job was not started because recent account payments have failed or your spending limit needs to be increased."}]`
			}
			workflow, _ := json.Marshal(map[string]string{"encoding": "base64", "content": base64.StdEncoding.EncodeToString([]byte("jobs:\n  build:\n    timeout-minutes: 30\n    runs-on: ubuntu-latest\n"))})
			responses := map[string]githubTestResponse{
				"gh api repos/test/repo/actions/runs/1":                            {stdout: `{"id":1,"head_sha":"abc","path":".github/workflows/ci.yml","status":"completed"}`},
				"gh api repos/test/repo/actions/runs/1/jobs?per_page=100&page=1":   {stdout: string(jobs)},
				"gh api repos/test/repo/check-runs/3/annotations?per_page=100":     {stdout: annotations},
				"gh api repos/test/repo/contents/.github/workflows/ci.yml?ref=abc": {stdout: string(workflow)},
			}
			host := New(githubTestCmdFactory(responses), func() bool { return true }, "", "test/repo")
			stalls, err := host.CIStalls(context.Background(), []scm.Check{{Name: "build", Bucket: scm.CheckBucketPending, Link: job.HTMLURL}}, now)
			if err != nil || len(stalls) != 1 {
				t.Fatalf("stalls=%#v err=%v", stalls, err)
			}
			if stalls[0].Class != class || stalls[0].JobKey != "build" || stalls[0].HeadSHA != "abc" {
				t.Fatalf("stall=%#v", stalls[0])
			}
		})
	}
}

func TestWorkloadRecoveryEligibilityPrivateOwnerOnly(t *testing.T) {
	for _, tc := range []struct {
		owner   string
		private bool
		want    bool
	}{
		{"bastotec", true, true}, {"brunobasto", true, true}, {"bastotec", false, false}, {"brunobasto", false, false}, {"other", true, false},
	} {
		t.Run(fmt.Sprintf("%s-%v", tc.owner, tc.private), func(t *testing.T) {
			slug := tc.owner + "/repo"
			responses := map[string]githubTestResponse{"gh api repos/" + slug: {stdout: fmt.Sprintf(`{"private":%v,"full_name":%q}`, tc.private, slug)}}
			host := New(githubTestCmdFactory(responses), func() bool { return true }, "", slug)
			got, err := host.WorkloadRecoveryAllowed(context.Background())
			if err != nil || got != tc.want {
				t.Fatalf("allowed=%v err=%v want=%v", got, err, tc.want)
			}
		})
	}
	host := New(failIfInvokedCmdFactory(t), func() bool { return true }, "ghe.example", "bastotec/repo")
	if allowed, err := host.WorkloadRecoveryAllowed(context.Background()); err != nil || allowed {
		t.Fatalf("enterprise allowed=%v %v", allowed, err)
	}
	host = NewWithFork(failIfInvokedCmdFactory(t), func() bool { return true }, "", "bastotec/repo", "contributor/repo", false)
	if allowed, err := host.WorkloadRecoveryAllowed(context.Background()); err != nil || allowed {
		t.Fatalf("fork allowed=%v %v", allowed, err)
	}
}

func TestRecoverCIStallNeverCancelsNonStalledSibling(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	jobs, _ := json.Marshal(map[string]any{"jobs": []stallJob{
		{ID: 2, Name: "build", Status: "queued", CreatedAt: now.Add(-time.Hour)},
		{ID: 3, Name: "test", Status: "in_progress", StartedAt: now.Add(-time.Minute)},
	}})
	workflow, _ := json.Marshal(map[string]string{"encoding": "base64", "content": base64.StdEncoding.EncodeToString([]byte("jobs:\n  build:\n    runs-on: ubuntu-latest\n  test:\n    timeout-minutes: 30\n    runs-on: ubuntu-latest\n"))})
	host := New(githubTestCmdFactory(map[string]githubTestResponse{
		"gh api repos/test/repo/actions/runs/1":                            {stdout: `{"head_sha":"abc","path":".github/workflows/ci.yml","status":"in_progress"}`},
		"gh api repos/test/repo/actions/runs/1/jobs?per_page=100&page=1":   {stdout: string(jobs)},
		"gh api repos/test/repo/contents/.github/workflows/ci.yml?ref=abc": {stdout: string(workflow)},
	}), func() bool { return true }, "", "test/repo")
	_, err := host.RecoverCIStall(context.Background(), scm.CIStall{HeadSHA: "abc", Active: true, Check: scm.Check{Link: "https://github.com/test/repo/actions/runs/1/job/2"}}, now)
	if !errors.Is(err, scm.ErrCIRecoveryUnsafe) {
		t.Fatalf("unsafe cancellation err=%v", err)
	}
}

func TestNoStartMessageOnlyProviderInfrastructurePhrases(t *testing.T) {
	for _, message := range []string{"recent account payments have failed", "spending limit needs to be increased", "no runner matched the specified labels"} {
		if !noStartMessage(message) {
			t.Fatal(message)
		}
	}
	for _, message := range []string{"test failed", "build timed out", "Set up job failed to download an action"} {
		if noStartMessage(message) {
			t.Fatal(message)
		}
	}
}
