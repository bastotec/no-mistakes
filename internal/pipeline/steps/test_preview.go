package steps

import (
	"fmt"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/scm/github"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func testPreviewMarker(runID string) string {
	return "<!-- no-mistakes:pr-validation:" + runID + " -->"
}

func PublishTestPreview(sctx *pipeline.StepContext) (*ipc.PublishTestPreviewResult, error) {
	if resolvedProvider(sctx) != scm.ProviderGitHub || resolvedHost(sctx, sctx.Repo.UpstreamURL) != "github.com" || existingPRURL(sctx) != "" {
		return nil, fmt.Errorf("Test previews currently require a run-owned github.com PR, not an explicit existing PR")
	}
	head, err := git.HeadSHA(sctx.Ctx, sctx.WorkDir)
	if err != nil {
		return nil, err
	}
	if head != sctx.Run.HeadSHA || !isFullGitObjectID(head) {
		return nil, fmt.Errorf("preview worktree does not match exact recorded head")
	}
	status, err := git.Run(sctx.Ctx, sctx.WorkDir, "status", "--porcelain", "--untracked-files=all")
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(status) != "" || mergeInProgress(sctx.Ctx, sctx.WorkDir) || rebaseInProgress(sctx.Ctx, sctx.WorkDir) {
		return nil, fmt.Errorf("preview refuses uncommitted or untracked repair work or an unfinished integration")
	}
	host, reason := buildHost(sctx, scm.ProviderGitHub)
	h, ok := host.(*github.Host)
	if !ok {
		return nil, fmt.Errorf("preview host unavailable: %s", reason)
	}
	if err := h.Available(sctx.Ctx); err != nil {
		return nil, err
	}
	branch := strings.TrimPrefix(sctx.Run.Branch, "refs/heads/")
	base := effectivePRBaseBranch(sctx)
	if branch == base {
		return nil, fmt.Errorf("preview requires a feature branch")
	}
	pr, err := h.FindPR(sctx.Ctx, branch, "")
	if err != nil {
		return nil, err
	}
	bound, err := sctx.DB.TestPreviewPR(sctx.Run.ID)
	if err != nil {
		return nil, err
	}
	if pr == nil && (bound != "" || runPRURL(sctx) != "") {
		return nil, fmt.Errorf("bound preview PR is missing or no longer open")
	}
	if pr != nil {
		if bound != "" && bound != pr.URL {
			return nil, fmt.Errorf("preview lookup returned a different PR")
		}
		if owned := runPRURL(sctx); owned != "" && owned != pr.URL {
			return nil, fmt.Errorf("run is already bound to a different PR")
		}
		content, err := h.GetPRContent(sctx.Ctx, pr)
		if err != nil {
			return nil, err
		}
		// Recover the narrow create-before-bind crash window using a run-specific
		// marker. Never convert/adopt a foreign or already-ready PR.
		if bound == "" && !strings.Contains(content.Body, testPreviewMarker(sctx.Run.ID)) {
			return nil, fmt.Errorf("existing PR is not this run's Test preview")
		}
		state, err := h.TestPreviewState(sctx.Ctx, pr)
		if err != nil {
			return nil, err
		}
		if !*state.IsDraft || string(state.AutoMerge) != "null" || pr.BaseBranch != base {
			return nil, fmt.Errorf("existing preview must remain draft with auto-merge disabled and the pinned base")
		}
		if err := sctx.DB.BindTestPreview(sctx.Run.ID, pr.URL); err != nil {
			return nil, err
		}
	}
	if err := sctx.DB.RequireTestPreviewValidation(sctx.Run.ID); err != nil {
		return nil, err
	}
	rows, err := sctx.DB.GetStepsByRun(sctx.Run.ID)
	if err != nil {
		return nil, err
	}
	// Retain the ordinary publication's review continuity, lease, custody,
	// pre-push attestation and durable remote binding, WITHOUT completing Push.
	if err := sctx.DB.SetRunPushActive(sctx.Run.ID, true); err != nil {
		return nil, err
	}
	defer func() { _ = sctx.DB.SetRunPushActive(sctx.Run.ID, false) }()
	if err := publishRunHead(sctx, head, "", rows); err != nil {
		return nil, err
	}
	summary, _, _ := (&PRStep{}).buildPipelineSections(sctx, scm.ProviderGitHub, false, false)
	if summary == "" {
		return nil, fmt.Errorf("cannot render pending pipeline evidence")
	}
	if pr == nil {
		body := testPreviewMarker(sctx.Run.ID) + "\n\n## PR validation preview\n\nTest is still pending. This draft is for PR validation only, not merge approval. The attestation below identifies the current head.\n\n" + summary
		content, err := composeOwnedPRContent(prOwnedBody{}, "PR validation preview", body, scm.MaxPRBodyChars(scm.ProviderGitHub))
		if err != nil {
			return nil, err
		}
		pr, err = h.CreateTestPreview(sctx.Ctx, branch, base, content.scmContent())
	}
	// Existing descriptions are never replaced here. publishRunHead already
	// restamped only their pipeline attestation before the push.
	if err != nil {
		return nil, err
	}
	if pr == nil || pr.URL == "" {
		return nil, fmt.Errorf("preview create returned no PR identity")
	}
	if err := sctx.DB.BindTestPreview(sctx.Run.ID, pr.URL); err != nil {
		return nil, err
	}
	if err := sctx.DB.UpdateRunPRURL(sctx.Run.ID, pr.URL); err != nil {
		return nil, err
	}
	sctx.Run.PRURL = &pr.URL
	if err := h.LabelTestPreview(sctx.Ctx, pr); err != nil {
		return nil, err
	}
	if err := h.VerifyTestPreview(sctx.Ctx, pr, head); err != nil {
		return nil, err
	}
	url, err := h.TestPreviewURL(sctx.Ctx, head)
	if err != nil {
		return nil, fmt.Errorf("preview PR %s published; current-head deployment lookup: %w", pr.URL, err)
	}
	sr, err := sctx.DB.GetStepResult(sctx.StepResultID)
	if err != nil {
		return nil, err
	}
	sctx.Log(fmt.Sprintf("PR validation preview published for %s: %s; Test remains %s", head, pr.URL, sr.Status))
	return &ipc.PublishTestPreviewResult{RunID: sctx.Run.ID, HeadSHA: head, PRURL: pr.URL, PreviewURL: url, Label: github.PRValidationLabel, TestStatus: string(sr.Status)}, nil
}

// readyTestPreview is reached only by the ordinary PR step after Test and the
// remaining pre-publication gates. The binding (not a user-editable label)
// decides whether this run owns the draft-to-ready transition.
func readyTestPreview(sctx *pipeline.StepContext, host scm.Host, pr *scm.PR) error {
	bound, err := sctx.DB.TestPreviewPR(sctx.Run.ID)
	if err != nil || bound == "" {
		return err
	}
	if pr == nil || pr.URL != bound {
		return fmt.Errorf("validated preview PR identity changed")
	}
	rows, err := sctx.DB.GetStepsByRun(sctx.Run.ID)
	if err != nil {
		return err
	}
	testComplete := false
	for _, row := range rows {
		if row.StepName == types.StepTest {
			testComplete = row.Status == types.StepStatusCompleted
		}
	}
	needed, err := sctx.DB.TestPreviewNeedsValidation(sctx.Run.ID)
	if err != nil {
		return err
	}
	if !testComplete || needed {
		return fmt.Errorf("cannot ready preview without successful post-publication Test execution")
	}
	if sctx.Config != nil && sctx.Config.Providers.GitHub.DraftPullRequests {
		return nil
	}
	h, ok := host.(*github.Host)
	if !ok {
		return fmt.Errorf("preview requires GitHub host")
	}
	return h.ReadyTestPreview(sctx.Ctx, pr, sctx.Run.HeadSHA)
}

func emptyTestPreviewBody(sctx *pipeline.StepContext, parts prOwnedBody) bool {
	return parts.managed && strings.TrimSpace(parts.before+parts.after) == "" && strings.Contains(parts.appendix, testPreviewMarker(sctx.Run.ID))
}
