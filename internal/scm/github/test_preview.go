package github

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/scm"
)

// PRValidationLabel is the explicit automation hook for repository-owned preview
// workflows. The preview remains a draft until normal publication reaches PR.
const PRValidationLabel = "pr-validation"

type PreviewState struct {
	IsDraft   *bool           `json:"isDraft"`
	State     string          `json:"state"`
	HeadSHA   string          `json:"headRefOid"`
	AutoMerge json.RawMessage `json:"autoMergeRequest"`
}

func (h *Host) OpenPRBodies(ctx context.Context, branch string) ([]string, error) {
	owner := h.forkOwner
	if owner == "" {
		owner = repoOwner(h.repo)
	}
	if owner == "" {
		return nil, fmt.Errorf("cannot list preview candidates without repository owner")
	}
	args := append([]string{"api", "--method", "GET", "repos/" + h.repo + "/pulls",
		"-f", "state=open", "-f", "head=" + owner + ":" + branch,
		"-f", "per_page=100", "--paginate", "--slurp"}, h.apiHostArgs()...)
	out, err := h.cmd(ctx, "gh", args...).Output()
	if err != nil {
		return nil, fmt.Errorf("list preview candidates: %w", err)
	}
	var pages [][]struct {
		Body json.RawMessage `json:"body"`
	}
	if err := json.Unmarshal(out, &pages); err != nil {
		return nil, err
	}
	if len(pages) == 0 {
		return nil, fmt.Errorf("missing preview candidate pages")
	}
	var bodies []string
	for _, page := range pages {
		if page == nil {
			return nil, fmt.Errorf("missing preview candidate list")
		}
		for _, pr := range page {
			var body string
			if err := json.Unmarshal(pr.Body, &body); err != nil {
				return nil, fmt.Errorf("read preview candidate body: %w", err)
			}
			bodies = append(bodies, body)
		}
	}
	return bodies, nil
}

func (h *Host) TestPreviewState(ctx context.Context, pr *scm.PR) (PreviewState, error) {
	var state PreviewState
	selector, err := prSelector(pr)
	if err != nil {
		return state, err
	}
	args := append([]string{"pr", "view", selector}, h.repoArgs()...)
	args = append(args, "--json", "isDraft,state,headRefOid,autoMergeRequest")
	out, err := h.cmd(ctx, "gh", args...).Output()
	if err != nil {
		return state, fmt.Errorf("read preview PR state: %w", err)
	}
	if err := json.Unmarshal(out, &state); err != nil {
		return state, err
	}
	if state.IsDraft == nil || state.State != "OPEN" || state.HeadSHA == "" || len(state.AutoMerge) == 0 {
		return state, fmt.Errorf("incomplete or non-open preview PR state")
	}
	return state, nil
}

func (h *Host) VerifyTestPreview(ctx context.Context, pr *scm.PR, head string) error {
	state, err := h.TestPreviewState(ctx, pr)
	if err != nil {
		return err
	}
	if !*state.IsDraft || string(state.AutoMerge) != "null" || state.HeadSHA != head {
		return fmt.Errorf("preview PR must be draft, have auto-merge disabled and match exact head %s", head)
	}
	return nil
}

func (h *Host) CreateTestPreview(ctx context.Context, branch, base string, content scm.PRContent) (*scm.PR, error) {
	copy := *h
	copy.draft = true
	return copy.CreatePR(ctx, branch, base, content)
}

func (h *Host) LabelTestPreview(ctx context.Context, pr *scm.PR) error {
	// Create if absent, but never overwrite a maintainer's existing label.
	args := append([]string{"api", "repos/" + h.repo + "/labels/" + PRValidationLabel}, h.apiHostArgs()...)
	if _, err := h.cmd(ctx, "gh", args...).Output(); err != nil {
		args = append([]string{"api", "--method", "POST", "repos/" + h.repo + "/labels", "-f", "name=" + PRValidationLabel, "-f", "color=5319e7", "-f", "description=PR validation preview; pipeline validation still gates publication"}, h.apiHostArgs()...)
		if out, err := h.cmd(ctx, "gh", args...).CombinedOutput(); err != nil {
			return fmt.Errorf("create PR validation label: %s: %w", strings.TrimSpace(string(out)), err)
		}
	}
	selector, err := prSelector(pr)
	if err != nil {
		return err
	}
	args = append([]string{"pr", "edit", selector, "--add-label", PRValidationLabel}, h.repoArgs()...)
	if out, err := h.cmd(ctx, "gh", args...).CombinedOutput(); err != nil {
		return fmt.Errorf("label preview PR: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}

// TestPreviewURL reports only successful GitHub deployments for the EXACT head,
// never a previous branch deployment. Provisioning belongs to repo automation.
func (h *Host) TestPreviewURL(ctx context.Context, head string) (string, error) {
	args := append([]string{"api", "--method", "GET", "repos/" + h.repo + "/deployments", "-f", "sha=" + head, "-f", "per_page=100"}, h.apiHostArgs()...)
	out, err := h.cmd(ctx, "gh", args...).Output()
	if err != nil {
		return "", err
	}
	var deployments []struct {
		ID          int64  `json:"id"`
		SHA         string `json:"sha"`
		Environment string `json:"environment"`
	}
	if err := json.Unmarshal(out, &deployments); err != nil {
		return "", err
	}
	for _, d := range deployments {
		if d.SHA != head || !strings.Contains(strings.ToLower(d.Environment), "preview") {
			continue
		}
		args = append([]string{"api", fmt.Sprintf("repos/%s/deployments/%d/statuses", h.repo, d.ID)}, h.apiHostArgs()...)
		out, err = h.cmd(ctx, "gh", args...).Output()
		if err != nil {
			return "", err
		}
		var statuses []struct {
			State string `json:"state"`
			URL   string `json:"environment_url"`
		}
		if err := json.Unmarshal(out, &statuses); err != nil {
			return "", err
		}
		if len(statuses) > 0 && statuses[0].State == "success" && strings.HasPrefix(statuses[0].URL, "https://") {
			return statuses[0].URL, nil
		}
	}
	return "", nil
}

func (h *Host) ReadyTestPreview(ctx context.Context, pr *scm.PR, head string) error {
	state, err := h.TestPreviewState(ctx, pr)
	if err != nil {
		return err
	}
	if state.HeadSHA != head || string(state.AutoMerge) != "null" {
		return fmt.Errorf("validated preview must match exact head and have auto-merge disabled")
	}
	if !*state.IsDraft {
		return nil // A previous PR-step attempt already promoted this bound PR.
	}
	selector, err := prSelector(pr)
	if err != nil {
		return err
	}
	args := append([]string{"pr", "ready", selector}, h.repoArgs()...)
	if out, err := h.cmd(ctx, "gh", args...).CombinedOutput(); err != nil {
		return fmt.Errorf("ready validated preview PR: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}

func (h *Host) apiHostArgs() []string {
	if h.host != "" {
		return []string{"--hostname", h.host}
	}
	return nil
}
