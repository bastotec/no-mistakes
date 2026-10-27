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
	Stall    scm.CIStall `json:"stall"`
	Phase    string      `json:"phase"`
	At       time.Time   `json:"at"`
	Affected []string    `json:"affected,omitempty"`
	Detail   string      `json:"detail,omitempty"`
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
	if action == types.ActionAutoFix {
		severity = types.FindingSeverityError
	}
	description := fmt.Sprintf("CI stall %s: %s (%s): %s", state.Stall.Class, state.Stall.Check.Name, state.Phase, state.Detail)
	if len(state.Affected) > 0 {
		description += "; workflow cancellation affected jobs: " + strings.Join(state.Affected, ", ")
	}
	return Finding{ID: workloadFindingPrefix + key, Severity: severity, Action: action, Category: types.FindingCategoryCICheck, Check: state.Stall.Check.Name, CheckID: state.Stall.Check.ProviderID, Description: description}
}

func stallOutcome(key string, state stallRecoveryState, action string) *pipeline.StepOutcome {
	return ciObservationOutcome(Findings{Summary: "CI stall recovery: " + state.Phase, Items: []Finding{stallFinding(key, state, action)}})
}

// recoverStalls runs before ordinary failure classification. Its persisted
// reservation is separate from configurable transient reruns: a stalled job
// earns exactly one attempt, including across daemon restarts and new heads.
func (s *CIStep) recoverStalls(sctx *pipeline.StepContext, host scm.Host, checks []scm.Check, now time.Time) (bool, *pipeline.StepOutcome, error) {
	provider, ok := host.(scm.CIStallRecovery)
	if !ok {
		return false, nil, nil
	}
	stalls, err := provider.CIStalls(sctx.Ctx, checks, now)
	if err != nil {
		return false, nil, err
	}
	byKey := map[string]scm.CIStall{}
	for _, stall := range stalls {
		byKey[stall.Key] = stall
	}
	// Recovery is only established by a green check from the current head,
	// never by disappearance, cancellation, or an unrelated same-named sibling.
	for key, state := range s.transientReruns.stalls {
		for _, check := range checks {
			if check.Name == state.Stall.Check.Name && check.WorkflowID == state.Stall.Check.WorkflowID && check.Bucket == scm.CheckBucketPass && state.Phase != "recovered" && state.Phase != "handed-off" {
				state.Phase = "recovered"
				state.Detail += "; current-head check passed"
				if err := s.saveStall(sctx, key, state); err != nil {
					return false, nil, err
				}
			}
		}
	}
	for _, stall := range stalls {
		state, spent := s.transientReruns.stalls[stall.Key]
		if !spent && s.transientReruns.used(stall.Check.Name) > 0 {
			state = stallRecoveryState{Stall: stall, Phase: "rerun", At: now, Detail: "existing transient rerun consumed this job's one automatic retry"}
			if err := s.saveStall(sctx, stall.Key, state); err != nil {
				return true, nil, err
			}
			return true, nil, nil
		}
		if !spent {
			published, err := publishedBranchHead(sctx)
			if err != nil {
				return true, nil, err
			}
			if published != sctx.Run.HeadSHA || stall.HeadSHA != published {
				return true, ciRerunHeadMismatchOutcome(sctx.Run.HeadSHA, published), nil
			}
			state = stallRecoveryState{Stall: stall, Phase: "rerun-reserved", At: now, Detail: "one targeted rerun reserved"}
			if err := s.saveStall(sctx, stall.Key, state); err != nil {
				return true, nil, err
			}
			affected, err := provider.RecoverCIStall(sctx.Ctx, stall, now)
			state.Affected = affected
			if err != nil {
				state.Detail = "targeted rerun failed: " + err.Error()
			} else {
				state.Detail = "one targeted rerun requested"
			}
			state.Phase = "rerun"
			if errors.Is(err, scm.ErrCIRecoveryUnsafe) {
				state.Phase = "handed-off"
			}
			if err := s.saveStall(sctx, stall.Key, state); err != nil {
				return true, nil, err
			}
			sctx.Log(stallFinding(stall.Key, state, types.ActionNoOp).Description)
			if state.Phase == "handed-off" {
				return true, stallOutcome(stall.Key, state, types.ActionAskUser), nil
			}
			// Even a rejected rerun spent its budget. Do not try again on this poll.
			return true, nil, nil
		}
		if state.Phase == "recovered" { // A later stall shares the already-spent allowance.
			state.Stall = stall
		}
		if state.Phase == "handed-off" {
			return true, stallOutcome(stall.Key, state, types.ActionAskUser), nil
		}
		if now.Sub(state.At) < stallReplacementGrace {
			return true, nil, nil
		}
		canRemedy := stall.Class == "queued-no-runner" || stall.Class == "provider-no-start"
		if canRemedy && (state.Phase == "rerun" || state.Phase == "rerun-reserved" || state.Phase == "recovered" || state.Phase == "remedy-requested") {
			allowed, err := provider.WorkloadRecoveryAllowed(sctx.Ctx)
			if err != nil {
				return true, nil, err
			}
			if allowed && stall.JobKey != "" {
				state.Stall = stall
				state.Phase = "remedy-requested"
				state.Detail += "; workload runner repair required"
				if err := s.saveStall(sctx, stall.Key, state); err != nil {
					return true, nil, err
				}
				return true, stallOutcome(stall.Key, state, types.ActionAutoFix), nil
			}
		}
		state.Phase = "handed-off"
		state.Detail += "; automatic recovery exhausted or safe workload remedy unavailable"
		if err := s.saveStall(sctx, stall.Key, state); err != nil {
			return true, nil, err
		}
		return true, stallOutcome(stall.Key, state, types.ActionAskUser), nil
	}
	// An accepted rerun can disappear from the rollup or finish cancelled rather
	// than produce another detectable stall. Bound that wait too.
	for key, state := range s.transientReruns.stalls {
		if state.Phase == "recovered" || state.Phase == "handed-off" {
			continue
		}
		if _, present := byKey[key]; present {
			continue
		}
		if now.Sub(state.At) < stallReplacementGrace {
			return true, nil, nil
		}
		pending := false
		for _, check := range checks {
			if check.Name == state.Stall.Check.Name && check.WorkflowID == state.Stall.Check.WorkflowID && check.Bucket == scm.CheckBucketPending {
				pending = true
			}
		}
		if pending {
			continue
		} // a newly running job is governed by its own budget
		state.Phase = "handed-off"
		state.Detail += "; no successful replacement observed after recovery"
		if err := s.saveStall(sctx, key, state); err != nil {
			return true, nil, err
		}
		return true, stallOutcome(key, state, types.ActionAskUser), nil
	}
	return false, nil, nil
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
	job := lookup(lookup(root.Content[0], "jobs"), jobKey)
	runner := lookup(job, "runs-on")
	if runner == nil || runner.Kind == yaml.AliasNode {
		return nil, fmt.Errorf("job has no supported runs-on")
	}
	lines := strings.Split(string(data), "\n")
	line := runner.Line - 1
	if line < 0 || line >= len(lines) {
		return nil, fmt.Errorf("runs-on line unavailable")
	}
	source := lines[line]
	colon := strings.Index(source, "runs-on:")
	if colon < 0 {
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
			return ciRepairResult{}, true, fmt.Errorf("workload recovery is not authorized by durable findings")
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
		if err != nil {
			return repair, true, err
		}
		state.Phase = "remedy"
		state.Detail += "; workload runner repair committed"
		err = s.saveStall(sctx, key, state)
		return repair, true, err
	}
	return ciRepairResult{}, false, nil
}
