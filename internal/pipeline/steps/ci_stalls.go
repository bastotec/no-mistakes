package steps

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/types"
	"gopkg.in/yaml.v3"
)

const stallReplacementGrace = 10 * time.Minute
const workloadFindingPrefix = "ci-workload-"

type stallRecoveryState struct {
	Stall       scm.CIStall `json:"stall"`
	Phase       string      `json:"phase"`
	At          time.Time   `json:"at"`
	Affected    []string    `json:"affected,omitempty"`
	Detail      string      `json:"detail,omitempty"`
	MonitorHead string      `json:"monitor_head,omitempty"`
	RemedySpent bool        `json:"remedy_spent,omitempty"`
	Retained    bool        `json:"retained,omitempty"`
}

func (s *CIStep) saveStall(sctx *pipeline.StepContext, key string, state stallRecoveryState) error {
	if s.stallBudgetErr != nil {
		return fmt.Errorf("CI recovery budget is unreadable: %w", s.stallBudgetErr)
	}
	if sctx.DB == nil || sctx.Run == nil {
		return fmt.Errorf("CI recovery requires durable run storage")
	}
	if s.transientReruns.stalls == nil {
		s.transientReruns.stalls = map[string]stallRecoveryState{}
	}
	previous, exists := s.transientReruns.stalls[key]
	s.transientReruns.stalls[key] = state
	if err := s.persistRerunBudget(sctx); err != nil {
		if exists {
			s.transientReruns.stalls[key] = previous
		} else {
			delete(s.transientReruns.stalls, key)
		}
		return err
	}
	if sctx.StepResultID != "" {
		row, err := sctx.DB.GetStepResult(sctx.StepResultID)
		if err != nil {
			return err
		}
		var findings Findings
		if row != nil && row.FindingsJSON != nil {
			findings, err = types.ParseFindingsJSON(*row.FindingsJSON)
			if err != nil {
				return err
			}
		}
		kept := findings.Items[:0]
		for _, item := range findings.Items {
			if item.ID != workloadFindingPrefix+key {
				kept = append(kept, item)
			}
		}
		findings.Items = append(kept, stallFinding(key, state, types.ActionNoOp))
		encoded, err := types.MarshalFindingsJSON(findings)
		if err != nil {
			return err
		}
		if err := sctx.DB.SetStepFindings(sctx.StepResultID, encoded); err != nil {
			return err
		}
	}
	return nil
}

func stallFinding(key string, state stallRecoveryState, action string) Finding {
	severity := types.FindingSeverityInfo
	if action == types.ActionAskUser {
		severity = types.FindingSeverityWarning
	}
	description := fmt.Sprintf("CI stall %s: %s (%s): %s", state.Stall.Class, state.Stall.Check.Name, state.Phase, state.Detail)
	if len(state.Affected) > 0 {
		description += "; workflow cancellation affected jobs: " + strings.Join(state.Affected, ", ")
	}
	return Finding{ID: workloadFindingPrefix + key, Severity: severity, Action: action, Category: types.FindingCategoryCICheck, Check: state.Stall.Check.Name, CheckID: state.Stall.Check.ProviderID, Description: description}
}

// recoverStalls runs before ordinary failure classification. Its persisted
// reservation is separate from configurable transient reruns: a stalled job
// earns exactly one attempt, including across daemon restarts and new heads.
func (s *CIStep) recoverStalls(sctx *pipeline.StepContext, host scm.Host, pr *scm.PR, checks []scm.Check, now time.Time) (bool, *pipeline.StepOutcome, error) {
	provider, ok := host.(scm.CIStallRecovery)
	if !ok {
		return false, nil, nil
	}
	inspection, err := provider.CIStalls(sctx.Ctx, checks, now)
	if err != nil {
		return true, nil, err
	}
	for key, state := range s.transientReruns.stalls {
		if state.Phase != "remedy" && state.Phase != "human-repair" && state.Phase != "replacement" {
			continue
		}
		published, err := publishedBranchHead(sctx)
		if err != nil {
			return true, nil, err
		}
		if published != sctx.Run.HeadSHA || (pr.HeadSHA != "" && pr.HeadSHA != published) {
			return true, ciRerunHeadMismatchOutcome(sctx.Run.HeadSHA, published), nil
		}
		if state.MonitorHead != published {
			state.MonitorHead = published
			state.At = now
			if err := s.saveStall(sctx, key, state); err != nil {
				return true, nil, err
			}
		}
	}
	byKey := map[string]scm.CIStall{}
	currentStalls := make([]scm.CIStall, 0, len(inspection.Stalls))
	for _, stall := range inspection.Stalls {
		state := s.transientReruns.stalls[stall.Key]
		if (state.Phase == "remedy" || state.Phase == "human-repair" || state.Phase == "replacement") && state.MonitorHead != "" && stall.HeadSHA != state.MonitorHead {
			continue
		}
		currentStalls = append(currentStalls, stall)
		byKey[stall.Key] = stall
	}
	inspection.Stalls = currentStalls
	for key, state := range s.transientReruns.stalls {
		if state.Phase == "recovered" {
			continue
		}
		resolved := false
		for _, check := range checks {
			if check.Name == state.Stall.Check.Name && check.WorkflowID == state.Stall.Check.WorkflowID && check.Bucket == scm.CheckBucketPass {
				resolved = true
			}
		}
		for _, executed := range inspection.Executed {
			previous, current := state.Stall.Check, executed.Check
			changed := current.Link != previous.Link || executed.HeadSHA != state.Stall.HeadSHA || current.CompletedAt.After(previous.CompletedAt) || (current.ExecutionID != "" && previous.ExecutionID != "" && current.ExecutionID != previous.ExecutionID)
			if executed.Key == key && executed.HeadSHA == sctx.Run.HeadSHA && changed {
				resolved = true
			}
		}
		if resolved {
			state.RemedySpent = state.RemedySpent || state.Phase == "remedy" || state.Phase == "remedy-reserved" || state.Phase == "human-repair"
			state.Phase = "recovered"
			state.Detail += "; current-head replacement executed conclusively"
			if err := s.saveStall(sctx, key, state); err != nil {
				return true, nil, err
			}
		}
	}
	for key, state := range s.transientReruns.stalls {
		if _, present := byKey[key]; present || state.Phase == "deferred" || state.Phase == "recovered" || state.Phase == "handed-off" || now.Sub(state.At) < stallReplacementGrace {
			continue
		}
		pending := false
		for _, check := range checks {
			if check.Name == state.Stall.Check.Name && check.WorkflowID == state.Stall.Check.WorkflowID && check.Bucket == scm.CheckBucketPending {
				pending = true
			}
		}
		if !pending {
			if state.MonitorHead != "" {
				state.Phase = "handed-off"
				state.Detail += "; current-head replacement did not register within monitoring grace"
				if err := s.saveStall(sctx, key, state); err != nil {
					return true, nil, err
				}
				continue
			}
			inspection.Stalls = append(inspection.Stalls, state.Stall)
			byKey[key] = state.Stall
		}
	}
	processed := map[string]bool{}
	waiting := false
	var handoffs []Finding
	for _, stall := range inspection.Stalls {
		if processed[stall.Key] {
			continue
		}
		state, spent := s.transientReruns.stalls[stall.Key]
		spent = spent && state.Phase != "deferred"
		if !spent && s.transientReruns.used(stall.Check.Name) > 0 {
			state = stallRecoveryState{Stall: stall, Phase: "rerun", At: now, Detail: "existing transient rerun consumed this job's one automatic retry"}
			if err := s.saveStall(sctx, stall.Key, state); err != nil {
				return true, nil, err
			}
			spent = true
		}
		if !spent {
			published, err := publishedBranchHead(sctx)
			if err != nil {
				return true, nil, err
			}
			if published != sctx.Run.HeadSHA || stall.HeadSHA != published {
				return true, ciRerunHeadMismatchOutcome(sctx.Run.HeadSHA, published), nil
			}
			plan, planErr := provider.PlanCIStallRecovery(sctx.Ctx, stall, now)
			if errors.Is(planErr, scm.ErrCIRecoveryUnsafe) {
				state = stallRecoveryState{Stall: stall, Phase: "deferred", At: now, Detail: "targeted rerun deferred: " + planErr.Error()}
				if err := s.saveStall(sctx, stall.Key, state); err != nil {
					return true, nil, err
				}
				waiting = true
				continue
			}
			if planErr != nil {
				return true, nil, planErr
			}
			if len(plan) == 0 {
				planErr = fmt.Errorf("provider returned an empty recovery plan")
			}
			for _, affected := range plan {
				if saved, used := s.transientReruns.stalls[affected.Key]; (used && saved.Phase != "deferred") || s.transientReruns.used(affected.Check.Name) > 0 {
					planErr = fmt.Errorf("affected job %q has already spent its retry", affected.Check.Name)
				}
				if affected.HeadSHA != published {
					planErr = fmt.Errorf("affected job head changed")
				}
			}
			if planErr != nil {
				state = stallRecoveryState{Stall: stall, Phase: "handed-off", At: now, Detail: "safe targeted rerun unavailable: " + planErr.Error()}
				if err := s.saveStall(sctx, stall.Key, state); err != nil {
					return true, nil, err
				}
				continue
			}
			var names []string
			for _, affected := range plan {
				names = append(names, affected.Check.Name)
			}
			for _, affected := range plan {
				reservation := stallRecoveryState{Stall: affected, Phase: "rerun-reserved", At: now, Detail: "one targeted rerun reserved"}
				if stall.Active {
					reservation.Affected = names
				}
				if err := s.saveStall(sctx, affected.Key, reservation); err != nil {
					return true, nil, err
				}
			}
			recoveryErr := provider.RecoverCIStall(sctx.Ctx, plan, now)
			for _, affected := range plan {
				processed[affected.Key] = true
				reservation := s.transientReruns.stalls[affected.Key]
				reservation.Phase = "rerun"
				reservation.Detail = "one targeted rerun requested"
				if recoveryErr != nil {
					reservation.Detail = "targeted rerun failed: " + recoveryErr.Error()
				}
				if errors.Is(recoveryErr, scm.ErrCIRecoveryUnsafe) {
					reservation.Phase = "deferred"
				}
				if err := s.saveStall(sctx, affected.Key, reservation); err != nil {
					return true, nil, err
				}
				sctx.Log(stallFinding(affected.Key, reservation, types.ActionNoOp).Description)
			}
			waiting = true
		}
	}
	for _, stall := range inspection.Stalls {
		state := s.transientReruns.stalls[stall.Key]
		if state.Phase == "handed-off" || state.Phase == "deferred" {
			continue
		}
		if now.Sub(state.At) < stallReplacementGrace {
			waiting = true
			continue
		}
		canRemedy := stall.Class == "queued-no-runner" || stall.Class == "provider-no-start"
		if canRemedy && !state.RemedySpent && (state.Phase == "rerun" || state.Phase == "rerun-reserved" || state.Phase == "recovered" || state.Phase == "replacement" || state.Phase == "remedy-requested") {
			state.Stall = stall
			state.Phase = "remedy-requested"
			state.Detail += "; workload runner repair required"
			if err := s.saveStall(sctx, stall.Key, state); err != nil {
				return true, nil, err
			}
			findings := Findings{Items: []Finding{stallFinding(stall.Key, state, types.ActionNoOp)}}
			repair, _, repairErr := s.workloadRepair(sctx, host, pr, findings)
			if outcome := pipeline.ProtectedPathOutcome(repairErr); outcome != nil {
				return true, outcome, nil
			}
			if repair.HeadAdvanced {
				s.pendingFixSummary = repair.Summary
				s.pendingRepairPublish = true
				if repair.Revalidate {
					if repairErr != nil {
						sctx.Log(fmt.Sprintf("warning: CI repair recorded for revalidation with unsettled recovery state: %v", repairErr))
					}
					return true, &pipeline.StepOutcome{RestartFrom: types.StepReview, Findings: sctx.DeferredFindings}, nil
				}
				if repairErr != nil {
					return true, nil, repairErr
				}
				return true, nil, nil
			}
			state = s.transientReruns.stalls[stall.Key]
			if repairErr != nil {
				state.Detail += "; workload repair failed: " + repairErr.Error()
			}
		}
		state.Stall = stall
		state.Phase = "handed-off"
		state.Detail += "; automatic recovery exhausted or safe workload remedy unavailable"
		if err := s.saveStall(sctx, stall.Key, state); err != nil {
			return true, nil, err
		}
	}
	for key, state := range s.transientReruns.stalls {
		if _, present := byKey[key]; present || state.Phase == "deferred" || state.Phase == "recovered" || state.Phase == "handed-off" {
			continue
		}
		if now.Sub(state.At) < stallReplacementGrace {
			waiting = true
		}
	}
	if waiting {
		return true, nil, nil
	}
	for key, state := range s.transientReruns.stalls {
		if state.Phase == "handed-off" {
			handoffs = append(handoffs, stallFinding(key, state, types.ActionAskUser))
		}
	}
	if len(handoffs) > 0 {
		sort.Slice(handoffs, func(i, j int) bool { return handoffs[i].ID < handoffs[j].ID })
		return false, ciObservationOutcome(Findings{Summary: "CI stall recovery needs attention", Items: handoffs}), nil
	}
	return waiting, nil, nil
}

func (s *CIStep) withoutHandedOffStallChecks(checks []scm.Check) []scm.Check {
	var remaining []scm.Check
	for _, check := range checks {
		handedOff := false
		for _, state := range s.transientReruns.stalls {
			previous := state.Stall.Check
			if state.Phase == "handed-off" && !check.AwaitingApproval() && check.Name == previous.Name && check.WorkflowID == previous.WorkflowID && (previous.ProviderID == "" || check.ProviderID == previous.ProviderID) {
				handedOff = true
				break
			}
		}
		if !handedOff {
			remaining = append(remaining, check)
		}
	}
	return remaining
}

func (s *CIStep) retainSelectedStallRepairs(sctx *pipeline.StepContext, findings Findings) error {
	for _, finding := range findings.Items {
		if !strings.HasPrefix(finding.ID, workloadFindingPrefix) {
			continue
		}
		key := strings.TrimPrefix(finding.ID, workloadFindingPrefix)
		state, ok := s.transientReruns.stalls[key]
		if !ok || state.Phase != "handed-off" {
			continue
		}
		state.Retained = true
		if err := s.saveStall(sctx, key, state); err != nil {
			return err
		}
	}
	return nil
}

func (s *CIStep) settleStallRepairs(sctx *pipeline.StepContext) error {
	if len(s.transientReruns.stalls) == 0 {
		return nil
	}
	findings, err := types.ParseFindingsJSON(sctx.PreviousFindings)
	if err != nil && sctx.PreviousFindings != "" {
		return err
	}
	selected := map[string]bool{}
	for _, finding := range findings.Items {
		if strings.HasPrefix(finding.ID, workloadFindingPrefix) {
			selected[strings.TrimPrefix(finding.ID, workloadFindingPrefix)] = true
		}
	}
	for key, state := range s.transientReruns.stalls {
		switch {
		case state.Phase == "remedy-reserved":
			state.Phase = "remedy"
			state.RemedySpent = true
			state.Detail += "; workload runner repair committed"
		case state.Phase == "handed-off" && (state.Retained || selected[key]):
			state.Phase = "human-repair"
			state.RemedySpent = true
			state.Detail += "; selected repair committed, awaiting replacement monitoring"
		case state.Phase == "recovered" || state.Phase == "rerun" || state.Phase == "rerun-reserved" || state.Phase == "remedy-requested":
			state.Phase = "replacement"
		case state.Phase != "remedy" && state.Phase != "human-repair" && state.Phase != "replacement":
			continue
		}
		state.Retained = false
		state.MonitorHead = ""
		state.At = time.Time{}
		if err := s.saveStall(sctx, key, state); err != nil {
			return err
		}
	}
	return nil
}

func (s *CIStep) appendStallFindings(outcome *pipeline.StepOutcome) {
	if outcome == nil || len(s.transientReruns.stalls) == 0 {
		return
	}
	findings, err := types.ParseFindingsJSON(outcome.Findings)
	if err != nil && outcome.Findings != "" {
		return
	}
	var keys []string
	for key := range s.transientReruns.stalls {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		duplicate := false
		for _, f := range findings.Items {
			if f.ID == workloadFindingPrefix+key {
				duplicate = true
			}
		}
		if !duplicate {
			findings.Items = append(findings.Items, stallFinding(key, s.transientReruns.stalls[key], types.ActionNoOp))
		}
	}
	outcome.Findings, _ = types.MarshalFindingsJSON(findings)
}

// rewriteWorkloadRunner deliberately supports only a single-line runs-on.
// Expressions, anchors, multiline values and symlinks fail closed rather than
// reserializing YAML or changing unrelated jobs and comments.
func rewriteWorkloadRunner(data []byte, jobKey string) ([]byte, error) {
	var root yaml.Node
	if err := yaml.Unmarshal(data, &root); err != nil {
		return nil, err
	}
	lookup := func(node *yaml.Node, key string) *yaml.Node {
		if node == nil || node.Kind != yaml.MappingNode {
			return nil
		}
		for i := 0; i+1 < len(node.Content); i += 2 {
			if node.Content[i].Value == key {
				return node.Content[i+1]
			}
		}
		return nil
	}
	if len(root.Content) != 1 {
		return nil, fmt.Errorf("workflow document unavailable")
	}
	jobs := lookup(root.Content[0], "jobs")
	job := lookup(jobs, jobKey)
	if root.Content[0].Style&yaml.FlowStyle != 0 || jobs == nil || jobs.Style&yaml.FlowStyle != 0 || job == nil || job.Style&yaml.FlowStyle != 0 {
		return nil, fmt.Errorf("flow-style workflow mappings are not eligible")
	}
	runner := lookup(job, "runs-on")
	if runner == nil || runner.Kind == yaml.AliasNode || runner.Style&(yaml.LiteralStyle|yaml.FoldedStyle) != 0 {
		return nil, fmt.Errorf("job has no supported runs-on")
	}
	lines := strings.Split(string(data), "\n")
	line := runner.Line - 1
	if line < 0 || line >= len(lines) {
		return nil, fmt.Errorf("runs-on line unavailable")
	}
	source := lines[line]
	colon := strings.Index(source, "runs-on:")
	if colon < 0 || strings.TrimSpace(source[:colon]) != "" {
		return nil, fmt.Errorf("runs-on must be on one line")
	}
	for _, child := range runner.Content {
		if child.Line != runner.Line {
			return nil, fmt.Errorf("multiline runs-on is not eligible")
		}
	}
	if runner.Kind != yaml.ScalarNode && runner.Kind != yaml.SequenceNode {
		return nil, fmt.Errorf("unsupported runs-on value")
	}
	if strings.Contains(source, "${{") || strings.Contains(source, "&") || strings.Contains(source, "*") || strings.HasSuffix(strings.TrimSpace(source), "|") || strings.HasSuffix(strings.TrimSpace(source), ">") {
		return nil, fmt.Errorf("dynamic runs-on is not eligible")
	}
	suffix := ""
	if hash := strings.Index(source, " #"); hash >= 0 {
		suffix = source[hash:]
	}
	lines[line] = source[:colon] + "runs-on: [self-hosted, linux, workload]" + suffix
	result := []byte(strings.Join(lines, "\n"))
	if string(result) == string(data) {
		return nil, fmt.Errorf("job already targets workload")
	}
	var validated map[string]any
	if err := yaml.Unmarshal(result, &validated); err != nil {
		return nil, fmt.Errorf("invalid rewritten workflow: %w", err)
	}
	return result, nil
}

func (s *CIStep) workloadRepair(sctx *pipeline.StepContext, host scm.Host, pr *scm.PR, findings Findings) (ciRepairResult, bool, error) {
	for _, finding := range findings.Items {
		if !strings.HasPrefix(finding.ID, workloadFindingPrefix) {
			continue
		}
		key := strings.TrimPrefix(finding.ID, workloadFindingPrefix)
		state, ok := s.transientReruns.stalls[key]
		if !ok || state.Phase != "remedy-requested" {
			return ciRepairResult{}, false, nil
		}
		provider, ok := host.(scm.CIStallRecovery)
		if !ok {
			return ciRepairResult{}, true, fmt.Errorf("provider has no workload recovery")
		}
		allowed, err := provider.WorkloadRecoveryAllowed(sctx.Ctx)
		if err != nil || !allowed {
			return ciRepairResult{}, true, fmt.Errorf("workload recovery repository is not eligible: %v", err)
		}
		if err := historyConstraintRefusal(sctx, host, pr); err != nil {
			return ciRepairResult{}, true, err
		}
		if err := sctx.DB.SetRunPushActive(sctx.Run.ID, true); err != nil {
			return ciRepairResult{}, true, err
		}
		defer func() { _ = sctx.DB.SetRunPushActive(sctx.Run.ID, false) }()
		published, err := publishedBranchHead(sctx)
		if err != nil || published != sctx.Run.HeadSHA || state.Stall.HeadSHA != published {
			return ciRepairResult{}, true, fmt.Errorf("workload recovery head changed")
		}
		status, err := stepGitRun(sctx, "status", "--porcelain")
		if err != nil || strings.TrimSpace(status) != "" {
			return ciRepairResult{}, true, fmt.Errorf("workload recovery requires a clean worktree")
		}
		p := state.Stall.WorkflowPath
		if !strings.HasPrefix(p, ".github/workflows/") || filepath.ToSlash(filepath.Clean(p)) != p || filepath.ToSlash(filepath.Dir(p)) != ".github/workflows" {
			return ciRepairResult{}, true, fmt.Errorf("invalid workflow path")
		}
		// git show reads the tracked object, not an agent-controlled symlink target.
		entry, err := stepGitRun(sctx, "ls-tree", "HEAD", "--", p)
		if err != nil || !strings.HasPrefix(entry, "100644 ") {
			return ciRepairResult{}, true, fmt.Errorf("workflow must be a regular tracked file")
		}
		data, err := stepGitRunRaw(sctx, "show", "HEAD:"+p)
		if err != nil {
			return ciRepairResult{}, true, err
		}
		replacement, err := rewriteWorkloadRunner([]byte(data), state.Stall.JobKey)
		if err != nil {
			return ciRepairResult{}, true, err
		}
		state.Phase = "remedy-reserved"
		state.RemedySpent = true
		state.At = time.Now()
		state.Detail += "; one workload repair reserved"
		if err := s.saveStall(sctx, key, state); err != nil {
			return ciRepairResult{}, true, err
		}
		// Reuse CI repair's protected-path, staging, continuity, revalidation and
		// publication rules; this path grants no alternative push authority.
		workdir := sctx.WorkDir
		for _, component := range []string{".github", ".github/workflows", p} {
			info, err := os.Lstat(filepath.Join(workdir, component))
			if err != nil || info.Mode()&os.ModeSymlink != 0 {
				return ciRepairResult{}, true, fmt.Errorf("workflow path must not contain symlinks")
			}
		}
		if err := os.WriteFile(filepath.Join(workdir, p), replacement, 0644); err != nil {
			return ciRepairResult{}, true, err
		}
		repair, err := s.commitRepair(sctx, "move stalled CI job to workload runner")
		return repair, true, err
	}
	return ciRepairResult{}, false, nil
}
