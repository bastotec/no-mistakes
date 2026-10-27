package github

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
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
			inspection, err := host.CIStalls(context.Background(), []scm.Check{{Name: "build", Bucket: scm.CheckBucketPending, Link: job.HTMLURL}}, now)
			stalls := inspection.Stalls
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
	_, err := host.PlanCIStallRecovery(context.Background(), scm.CIStall{HeadSHA: "abc", WorkflowPath: ".github/workflows/ci.yml", Active: true, Check: scm.Check{Link: "https://github.com/test/repo/actions/runs/1/job/2"}}, now)
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

func TestCIStallsCachesMetadataPerInspectionAndRecognizesExecutedFailure(t *testing.T) {
	now := time.Now()
	jobs := []stallJob{}
	checks := []scm.Check{}
	for i := 2; i < 5; i++ {
		link := fmt.Sprintf("https://github.com/test/repo/actions/runs/1/job/%d", i)
		jobs = append(jobs, stallJob{ID: int64(i), Name: fmt.Sprintf("job%d", i), Status: "completed", Conclusion: "failure", HTMLURL: link, Steps: []json.RawMessage{json.RawMessage(`{"number":1,"status":"completed","conclusion":"success"}`), json.RawMessage(`{"number":2,"status":"completed","conclusion":"failure"}`)}})
		checks = append(checks, scm.Check{Name: fmt.Sprintf("job%d", i), Link: link, Bucket: scm.CheckBucketFail})
	}
	payload, _ := json.Marshal(map[string]any{"jobs": jobs})
	workflow, _ := json.Marshal(map[string]string{"encoding": "base64", "content": base64.StdEncoding.EncodeToString([]byte("jobs:\n  job2: {}\n  job3: {}\n  job4: {}\n"))})
	responses := map[string]githubTestResponse{
		"gh api repos/test/repo/actions/runs/1":                            {stdout: `{"head_sha":"abc","path":".github/workflows/ci.yml","status":"completed"}`},
		"gh api repos/test/repo/actions/runs/1/jobs?per_page=100&page=1":   {stdout: string(payload)},
		"gh api repos/test/repo/contents/.github/workflows/ci.yml?ref=abc": {stdout: string(workflow)},
	}
	counts := map[string]int{}
	host := New(func(ctx context.Context, name string, args ...string) *exec.Cmd {
		counts[strings.Join(args, " ")]++
		return githubTestCmdFactory(responses)(ctx, name, args...)
	}, func() bool { return true }, "", "test/repo")
	for round := 1; round <= 2; round++ {
		inspection, err := host.CIStalls(context.Background(), checks, now)
		if err != nil || len(inspection.Stalls) != 0 || len(inspection.Executed) != len(checks) {
			t.Fatalf("inspection=%#v err=%v", inspection, err)
		}
		if len(counts) != 3 {
			t.Fatalf("unexpected metadata reads: %#v", counts)
		}
		for key, count := range counts {
			if count != round {
				t.Fatalf("%s read %d times after %d inspections", key, count, round)
			}
		}
	}
}

func TestRecoverCIStallCancelsOnceAndRetriesEveryReservedJob(t *testing.T) {
	now := time.Now()
	jobs, _ := json.Marshal(map[string]any{"jobs": []stallJob{
		{ID: 2, Name: "build", Status: "queued", CreatedAt: now.Add(-time.Hour)},
		{ID: 3, Name: "test", Status: "queued", CreatedAt: now.Add(-time.Hour)},
	}})
	workflow, _ := json.Marshal(map[string]string{"encoding": "base64", "content": base64.StdEncoding.EncodeToString([]byte("jobs:\n  build:\n    runs-on: ubuntu-latest\n  test:\n    runs-on: ubuntu-latest\n"))})
	responses := map[string]githubTestResponse{
		"gh api repos/test/repo/actions/runs/1":                            {stdout: `{"head_sha":"abc","path":".github/workflows/ci.yml","status":"in_progress"}`},
		"gh api repos/test/repo/actions/runs/1/jobs?per_page=100&page=1":   {stdout: string(jobs)},
		"gh api repos/test/repo/contents/.github/workflows/ci.yml?ref=abc": {stdout: string(workflow)},
		"gh run cancel 1 --repo test/repo":                                 {},
		"gh run rerun --job 2 --repo test/repo":                            {},
		"gh run rerun --job 3 --repo test/repo":                            {},
	}
	var mutations []string
	host := New(func(ctx context.Context, name string, args ...string) *exec.Cmd {
		key := strings.TrimSpace(name + " " + strings.Join(args, " "))
		cmd := githubTestCmdFactory(responses)(ctx, name, args...)
		if strings.HasPrefix(key, "gh run ") {
			mutations = append(mutations, key)
			if key == "gh run cancel 1 --repo test/repo" {
				responses["gh api repos/test/repo/actions/runs/1"] = githubTestResponse{stdout: `{"head_sha":"abc","path":".github/workflows/ci.yml","status":"completed"}`}
			}
		}
		return cmd
	}, func() bool { return true }, "", "test/repo")
	stall := scm.CIStall{HeadSHA: "abc", WorkflowPath: ".github/workflows/ci.yml", Active: true, Check: scm.Check{Name: "build", Link: "https://github.com/test/repo/actions/runs/1/job/2"}}
	plan, err := host.PlanCIStallRecovery(context.Background(), stall, now)
	if err != nil || len(plan) != 2 || len(mutations) != 0 {
		t.Fatalf("plan=%#v err=%v mutations=%#v", plan, err, mutations)
	}
	if err := host.RecoverCIStall(context.Background(), plan, now); err != nil {
		t.Fatal(err)
	}
	want := "gh run cancel 1 --repo test/repo\ngh run rerun --job 2 --repo test/repo\ngh run rerun --job 3 --repo test/repo"
	if strings.Join(mutations, "\n") != want {
		t.Fatalf("mutations=%#v", mutations)
	}
}

func TestRecoverCIStallRevalidatesAffectedJobsBeforeCancellation(t *testing.T) {
	now := time.Now()
	jobs, _ := json.Marshal(map[string]any{"jobs": []stallJob{
		{ID: 2, Name: "build", Status: "queued", CreatedAt: now.Add(-time.Hour)},
		{ID: 3, Name: "test", Status: "queued", CreatedAt: now.Add(-time.Hour)},
	}})
	workflow, _ := json.Marshal(map[string]string{"encoding": "base64", "content": base64.StdEncoding.EncodeToString([]byte("jobs:\n  build: {}\n  test: {}\n"))})
	host := New(githubTestCmdFactory(map[string]githubTestResponse{
		"gh api repos/test/repo/actions/runs/1":                            {stdout: `{"head_sha":"abc","path":".github/workflows/ci.yml","status":"in_progress"}`},
		"gh api repos/test/repo/actions/runs/1/jobs?per_page=100&page=1":   {stdout: string(jobs)},
		"gh api repos/test/repo/contents/.github/workflows/ci.yml?ref=abc": {stdout: string(workflow)},
	}), func() bool { return true }, "", "test/repo")
	plan := []scm.CIStall{{Key: ".github/workflows/ci.yml/build", HeadSHA: "abc", WorkflowPath: ".github/workflows/ci.yml", Active: true, Check: scm.Check{Name: "build", Link: "https://github.com/test/repo/actions/runs/1/job/2"}}}
	if err := host.RecoverCIStall(context.Background(), plan, now); !errors.Is(err, scm.ErrCIRecoveryUnsafe) {
		t.Fatalf("incomplete reservations authorized cancellation: %v", err)
	}
}
