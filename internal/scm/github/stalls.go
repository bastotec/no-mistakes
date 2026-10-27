package github

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/scm"
	"gopkg.in/yaml.v3"
)

// A queue is not stalled as soon as it appears. Ten minutes without an assigned
// runner earns one recovery; environment approvals and concurrency waits do not.
const queueStallThreshold = 10 * time.Minute

type stallRun struct {
	ID           int64     `json:"id"`
	HeadSHA      string    `json:"head_sha"`
	Path         string    `json:"path"`
	Status       string    `json:"status"`
	CreatedAt    time.Time `json:"created_at"`
	CheckSuiteID int64     `json:"check_suite_id"`
}
type stallJob struct {
	ID          int64             `json:"id"`
	Name        string            `json:"name"`
	Status      string            `json:"status"`
	Conclusion  string            `json:"conclusion"`
	RunnerID    int64             `json:"runner_id"`
	CreatedAt   time.Time         `json:"created_at"`
	StartedAt   time.Time         `json:"started_at"`
	CheckRunURL string            `json:"check_run_url"`
	HTMLURL     string            `json:"html_url"`
	Steps       []json.RawMessage `json:"steps"`
}
type stallWorkflow struct {
	Jobs map[string]struct {
		Name    string `yaml:"name"`
		Timeout string `yaml:"timeout-minutes"`
	} `yaml:"jobs"`
}

func (h *Host) stallAPI(ctx context.Context, endpoint string, target any) error {
	args := []string{"api"}
	if h.host != "" {
		args = append(args, "--hostname", h.host)
	}
	args = append(args, endpoint)
	out, err := h.cmd(ctx, "gh", args...).Output()
	if err != nil {
		return fmt.Errorf("read CI recovery metadata: %w", err)
	}
	if err := json.Unmarshal(out, target); err != nil {
		return fmt.Errorf("decode CI recovery metadata: %w", err)
	}
	return nil
}

func (h *Host) stallJobs(ctx context.Context, runID string) ([]stallJob, error) {
	// Pagination is mandatory: an unseen healthy matrix sibling must prevent
	// cancellation just as a sibling on page one does.
	var jobs []stallJob
	for page := 1; ; page++ {
		var payload struct {
			Jobs []stallJob `json:"jobs"`
		}
		if err := h.stallAPI(ctx, fmt.Sprintf("repos/%s/actions/runs/%s/jobs?per_page=100&page=%d", h.repoSlug(), runID, page), &payload); err != nil {
			return nil, err
		}
		jobs = append(jobs, payload.Jobs...)
		if len(payload.Jobs) < 100 {
			return jobs, nil
		}
	}
}

func (h *Host) stallWorkflow(ctx context.Context, run stallRun) (stallWorkflow, error) {
	var workflow stallWorkflow
	if !validWorkflowPath(run.Path) || run.HeadSHA == "" {
		return workflow, fmt.Errorf("workflow identity unavailable")
	}
	var payload struct {
		Content  string `json:"content"`
		Encoding string `json:"encoding"`
	}
	endpoint := "repos/" + h.repoSlug() + "/contents/" + run.Path + "?ref=" + url.QueryEscape(run.HeadSHA)
	if err := h.stallAPI(ctx, endpoint, &payload); err != nil {
		return workflow, err
	}
	if payload.Encoding != "base64" {
		return workflow, fmt.Errorf("unsupported workflow encoding")
	}
	data, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(payload.Content, "\n", ""))
	if err != nil {
		return workflow, err
	}
	err = yaml.Unmarshal(data, &workflow)
	return workflow, err
}

func validWorkflowPath(p string) bool {
	return strings.HasPrefix(p, ".github/workflows/") && path.Clean(p) == p && path.Dir(p) == ".github/workflows" && (strings.HasSuffix(p, ".yml") || strings.HasSuffix(p, ".yaml"))
}

func workflowJob(w stallWorkflow, name string) (string, time.Duration) {
	key := ""
	for id, job := range w.Jobs {
		display := job.Name
		if display == "" {
			display = id
		}
		if name != display && !strings.HasPrefix(name, display+" (") {
			continue
		}
		if key != "" {
			return "", 0
		} // never guess between same-named jobs
		key = id
	}
	if key == "" {
		return "", 0
	}
	timeout := 360 * time.Minute // GitHub's default timeout-minutes
	if value := w.Jobs[key].Timeout; value != "" {
		minutes, err := strconv.Atoi(value)
		if err != nil || minutes <= 0 {
			return key, 0
		} // expressions are not a proven budget
		timeout = time.Duration(minutes) * time.Minute
	}
	return key, timeout
}

func classifyStalledJob(job stallJob, timeout time.Duration, now time.Time) string {
	switch job.Status {
	case "queued":
		if job.RunnerID == 0 && !job.CreatedAt.IsZero() && now.Sub(job.CreatedAt) >= queueStallThreshold {
			return "queued-no-runner"
		}
	case "in_progress":
		if timeout > 0 && !job.StartedAt.IsZero() && now.Sub(job.StartedAt) >= timeout {
			return "running-past-timeout"
		}
	case "completed":
		if strings.EqualFold(job.Conclusion, "timed_out") {
			return "running-past-timeout"
		}
	}
	return ""
}

func noStartMessage(text string) bool {
	text = strings.ToLower(text)
	for _, phrase := range []string{"recent account payments have failed", "spending limit", "no runner matched", "no runner matching", "no runners matched", "no runner available", "no available runner"} {
		if strings.Contains(text, phrase) {
			return true
		}
	}
	return false
}

func (h *Host) noStartAnnotation(ctx context.Context, run stallRun, check scm.Check, job *stallJob) (bool, error) {
	var ids []int64
	if job != nil && job.CheckRunURL != "" {
		id, err := strconv.ParseInt(path.Base(job.CheckRunURL), 10, 64)
		if err == nil {
			ids = append(ids, id)
		}
	} else if run.CheckSuiteID != 0 {
		var payload struct {
			CheckRuns []struct {
				ID   int64  `json:"id"`
				Name string `json:"name"`
			} `json:"check_runs"`
		}
		if err := h.stallAPI(ctx, fmt.Sprintf("repos/%s/check-suites/%d/check-runs?per_page=100", h.repoSlug(), run.CheckSuiteID), &payload); err != nil {
			return false, err
		}
		for _, c := range payload.CheckRuns {
			if c.Name == check.Name || check.Kind == scm.CheckKindRun {
				ids = append(ids, c.ID)
			}
		}
	}
	for _, id := range ids {
		var annotations []struct {
			Message string `json:"message"`
		}
		if err := h.stallAPI(ctx, fmt.Sprintf("repos/%s/check-runs/%d/annotations?per_page=100", h.repoSlug(), id), &annotations); err != nil {
			return false, err
		}
		for _, a := range annotations {
			if noStartMessage(a.Message) {
				return true, nil
			}
		}
	}
	return false, nil
}

// CIStalls reads structural execution/runner facts and provider-authored
// annotations, never repository log prose. Workflow bytes are pinned to the
// execution's head, not a moving default branch.
func (h *Host) CIStalls(ctx context.Context, checks []scm.Check, now time.Time) ([]scm.CIStall, error) {
	var result []scm.CIStall
	for _, check := range checks {
		if check.Bucket == scm.CheckBucketPass || check.Bucket == scm.CheckBucketSkip || check.Bucket == scm.CheckBucketAwaitingApproval {
			continue
		}
		runID, jobID, ok := h.actionsRerunTarget(check.Link)
		if !ok {
			continue
		}
		var run stallRun
		if err := h.stallAPI(ctx, "repos/"+h.repoSlug()+"/actions/runs/"+runID, &run); err != nil {
			return nil, err
		}
		jobs, err := h.stallJobs(ctx, runID)
		if err != nil {
			return nil, err
		}
		var job *stallJob
		for i := range jobs {
			if (jobID != "" && strconv.FormatInt(jobs[i].ID, 10) == jobID) || (jobID == "" && jobs[i].Name == check.Name) {
				job = &jobs[i]
				break
			}
		}
		class, jobKey := "", ""
		// A failed setup or test is NOT no-start: only an execution with no
		// repository steps is eligible for the provider annotation classification.
		if job == nil || len(job.Steps) == 0 {
			noStart, err := h.noStartAnnotation(ctx, run, check, job)
			if err != nil {
				return nil, err
			}
			if noStart {
				class = "provider-no-start"
			}
		}
		workflow, workflowErr := h.stallWorkflow(ctx, run)
		if job != nil {
			var budget time.Duration
			if workflowErr == nil {
				jobKey, budget = workflowJob(workflow, job.Name)
			}
			if class == "" {
				class = classifyStalledJob(*job, budget, now)
			}
			if job.HTMLURL != "" {
				check.Link = job.HTMLURL
			}
		} else if workflowErr == nil && len(workflow.Jobs) == 1 {
			for id := range workflow.Jobs {
				jobKey = id
			}
		}
		if class == "" {
			continue
		}
		// No-start workflows have no job link; rerun the failed workflow using the
		// existing run-level fallback rather than inventing a job database id.
		result = append(result, scm.CIStall{Key: run.Path + "/" + check.Name, Class: class, Reason: class, Check: check, WorkflowPath: run.Path, JobKey: jobKey, HeadSHA: run.HeadSHA, Active: run.Status != "completed"})
	}
	return result, nil
}

func (h *Host) WorkloadRecoveryAllowed(ctx context.Context) (bool, error) {
	if h.forkOwner != "" || (h.host != "" && !strings.EqualFold(h.host, "github.com")) {
		return false, nil
	}
	owner := strings.Split(h.repoSlug(), "/")[0]
	if !strings.EqualFold(owner, "bastotec") && !strings.EqualFold(owner, "brunobasto") {
		return false, nil
	}
	var repo struct {
		Private  bool   `json:"private"`
		FullName string `json:"full_name"`
	}
	if err := h.stallAPI(ctx, "repos/"+h.repoSlug(), &repo); err != nil {
		return false, err
	}
	return repo.Private && strings.EqualFold(repo.FullName, h.repoSlug()), nil
}

func (h *Host) RecoverCIStall(ctx context.Context, stall scm.CIStall, now time.Time) ([]string, error) {
	runID, _, ok := h.actionsRerunTarget(stall.Check.Link)
	if !ok {
		return nil, fmt.Errorf("stalled job has no rerun target")
	}
	var affected []string
	if stall.Active {
		var run stallRun
		if err := h.stallAPI(ctx, "repos/"+h.repoSlug()+"/actions/runs/"+runID, &run); err != nil {
			return nil, err
		}
		if run.HeadSHA != stall.HeadSHA {
			return nil, fmt.Errorf("workflow head changed before cancellation")
		}
		jobs, err := h.stallJobs(ctx, runID)
		if err != nil {
			return nil, err
		}
		workflow, err := h.stallWorkflow(ctx, run)
		if err != nil {
			return nil, err
		}
		for _, job := range jobs {
			if job.Status == "completed" {
				continue
			}
			_, budget := workflowJob(workflow, job.Name)
			if classifyStalledJob(job, budget, now) == "" {
				return nil, fmt.Errorf("%w: %q", scm.ErrCIRecoveryUnsafe, job.Name)
			}
			affected = append(affected, job.Name)
		}
		if len(affected) == 0 {
			return nil, fmt.Errorf("active workflow has no proven stalled jobs; not cancelling")
		}
		args := append([]string{"run", "cancel", runID}, h.repoArgs()...)
		if _, err := h.cmd(ctx, "gh", args...).CombinedOutput(); err != nil {
			return nil, fmt.Errorf("cancel stalled workflow: %w", err)
		}
		// Cancellation is asynchronous. A bounded wait avoids a rejected rerun
		// request racing the cancellation; a timeout hands off rather than retrying.
		settled := false
		for i := 0; i < 15; i++ {
			if err := h.stallAPI(ctx, "repos/"+h.repoSlug()+"/actions/runs/"+runID, &run); err != nil {
				return affected, err
			}
			if run.Status == "completed" {
				settled = true
				break
			}
			select {
			case <-ctx.Done():
				return affected, ctx.Err()
			case <-time.After(time.Second):
			}
		}
		if !settled {
			return affected, fmt.Errorf("workflow cancellation did not settle before rerun")
		}
	}
	return affected, h.RerunCheck(ctx, nil, stall.Check)
}
