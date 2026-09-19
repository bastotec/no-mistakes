// Command jev-tally measures the advisory Jev signal's false-positive and
// false-negative rate against this repository's own recent merged pull
// requests. It exists so the operator can judge the signal's precision BEFORE
// trusting it as a reviewer aid; it is a local measurement tool, not part of
// the shipped gate behavior.
//
// Ground truth (printed in the report header) is deliberately the pipeline's
// own recorded outcome per PR, not a human re-review:
//
//	positive  = the no-mistakes PR summary shows real findings (issues found
//	            with a count > 0, an auto-fix round, or a fix-review gate)
//	negative  = an attested no-mistakes PR with no such marker
//	unknown   = no no-mistakes attestation in the body (excluded from rates)
//
// FP = flagged on a negative PR; FN = clear on a positive PR. Uncertain
// verdicts count as neither (they are the signal saying "ask a human", which
// is what the step reports and never acts on).
//
// The gateway key is resolved by reference exactly like the pipeline step
// (jev.ResolveKey) and is never written to the report, the state file, or
// stdout. A cached PR (same repo, number, and diff bytes) is never re-evaluated,
// so re-running the tally spends quota only on new PRs.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/jev"
)

type prRecord struct {
	Number int    `json:"number"`
	Title  string `json:"title"`
	Body   string `json:"body"`
}

// row is one measured PR: the record, the signal's verdict (empty when the
// PR could not be evaluated), per-question probabilities, and either the
// ground-truth label or why it was excluded.
type row struct {
	pr      prRecord
	verdict string
	answers map[string]float64
	note    string
}

type evalRecord struct {
	DiffSHA     string             `json:"diff_sha"`
	Verdict     string             `json:"verdict"`
	Answers     map[string]float64 `json:"answers"`
	EvaluatedAt time.Time          `json:"evaluated_at"`
	Error       string             `json:"error,omitempty"`
}

type stateFile struct {
	PRs map[string]evalRecord `json:"prs"`
}

var issuesFoundRe = regexp.MustCompile(`(\d+) issues? found`)
var autoFixedRe = regexp.MustCompile(`auto-fixed`)

// classifyGroundTruth reads the no-mistakes markers out of a PR body. A PR
// without a pipeline attestation returns ok=false: it says nothing about the
// signal either way. A positive with auto-fix rounds is labelled separately
// because the harness evaluates the MERGED diff: after the pipeline's own
// fixes landed, so "clear" on such a PR is the expected reading of a clean
// final diff, not evidence of a miss.
func classifyGroundTruth(body string) (label string, ok bool) {
	if !strings.Contains(body, "no-mistakes-pipeline-attestation") &&
		!strings.Contains(body, "no-mistakes pipeline") {
		return "", false
	}
	if autoFixedRe.MatchString(body) {
		return "positive (auto-fixed)", true
	}
	for _, m := range issuesFoundRe.FindAllStringSubmatch(body, -1) {
		if m[1] != "0" {
			return "positive", true
		}
	}
	return "negative", true
}

func runGH(args ...string) (string, error) {
	cmd := exec.Command("gh", args...)
	var out, errBuf strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("gh %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(errBuf.String()))
	}
	return out.String(), nil
}

func main() {
	repo := flag.String("repo", "", "owner/name (default: resolved by gh from the current directory)")
	limit := flag.Int("limit", 30, "how many recent merged PRs to measure")
	out := flag.String("out", "", "write the markdown report here (default: stdout)")
	statePath := flag.String("state-file", filepath.Join(os.Getenv("HOME"), ".no-mistakes", "jev-tally-state.json"), "evaluation cache so re-runs spend no quota")
	keyEnv := flag.String("key-env", jev.DefaultKeyEnv, "environment variable holding the gateway key")
	secrets := flag.String("secrets-file", jev.DefaultSecretsFile, "parsed fallback secrets file for the gateway key")
	model := flag.String("model", jev.DefaultModel, "evaluation model id (Gemini is refused)")
	timeout := flag.Duration("timeout", 30*time.Second, "per-evaluation timeout")
	flag.Parse()

	if err := run(*repo, *limit, *out, *statePath, *keyEnv, *secrets, *model, *timeout); err != nil {
		fmt.Fprintln(os.Stderr, "jev-tally:", err)
		os.Exit(1)
	}
}

func run(repo string, limit int, outPath, statePath, keyEnv, secretsFile, model string, timeout time.Duration) error {
	if limit <= 0 {
		return fmt.Errorf("--limit must be positive")
	}
	if jev.IsGeminiModel(model) {
		return fmt.Errorf("model %q names a Gemini model; refusing", model)
	}
	if strings.TrimSpace(repo) == "" {
		resolved, err := runGH("repo", "view", "--json", "nameWithOwner", "-q", ".nameWithOwner")
		if err != nil {
			return fmt.Errorf("--repo is required when gh cannot resolve it from the current directory: %w", err)
		}
		repo = strings.TrimSpace(resolved)
	}

	key, err := jev.ResolveKey(keyEnv, secretsFile)
	if err != nil || key == "" {
		return fmt.Errorf("gateway key not found: set %s or add it to %s (by reference; the key itself is never stored or logged)", keyEnv, secretsFile)
	}

	listJSON, err := runGH("pr", "list", "-R", repo, "--state", "merged", "--limit", fmt.Sprintf("%d", limit), "--json", "number,title,body")
	if err != nil {
		return fmt.Errorf("list merged PRs: %w", err)
	}
	var prs []prRecord
	if err := json.Unmarshal([]byte(listJSON), &prs); err != nil {
		return fmt.Errorf("parse gh pr list: %w", err)
	}
	sort.Slice(prs, func(i, j int) bool { return prs[i].Number > prs[j].Number })

	state, err := loadState(statePath)
	if err != nil {
		return fmt.Errorf("load state: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(len(prs)+1)*timeout)
	defer cancel()
	client := jev.NewClient(key, jev.WithModel(model))
	questions := jev.AdvisoryQuestions()

	rows := make([]row, 0, len(prs))
	evaluated := 0
	for _, pr := range prs {
		label, known := classifyGroundTruth(pr.Body)
		if !known {
			rows = append(rows, row{pr: pr, note: "no no-mistakes attestation"})
			continue
		}
		diff, err := runGH("pr", "diff", "-R", repo, fmt.Sprintf("%d", pr.Number))
		if err != nil {
			rows = append(rows, row{pr: pr, note: "diff unavailable"})
			continue
		}
		if strings.TrimSpace(diff) == "" {
			rows = append(rows, row{pr: pr, note: "empty diff"})
			continue
		}
		sum := sha256.Sum256([]byte(diff))
		diffSHA := fmt.Sprintf("%x", sum)
		cacheKey := fmt.Sprintf("%s#%d", repo, pr.Number)

		var verdict string
		var answers map[string]float64
		if cached, ok := state.PRs[cacheKey]; ok && cached.DiffSHA == diffSHA && cached.Error == "" {
			verdict, answers = cached.Verdict, cached.Answers
		} else {
			stateDiff := diff
			if len(stateDiff) > jev.MaxStateBytes {
				stateDiff = stateDiff[:jev.MaxStateBytes] + "\n[diff truncated]\n"
			}
			// Unlike the pipeline step (which deliberately never retries), a
			// measurement tool retries one transient-looking failure so a
			// blip does not silently shrink the sample.
			var result *jev.Result
			var evalErr error
			for attempt := 0; attempt < 2; attempt++ {
				result, evalErr = client.Evaluate(ctx, stateDiff, questions)
				if evalErr == nil || !transient(evalErr.Error()) {
					break
				}
				select {
				case <-ctx.Done():
					evalErr = ctx.Err()
				case <-time.After(5 * time.Second):
				}
			}
			if evalErr != nil {
				rows = append(rows, row{pr: pr, note: "evaluate error: " + evalErr.Error()})
				continue
			}
			verdict = jev.Verdict(result, questions, 0.6)
			answers = map[string]float64{}
			for name, ans := range result.Answers {
				answers[name] = ans.Probability
			}
			state.PRs[cacheKey] = evalRecord{DiffSHA: diffSHA, Verdict: verdict, Answers: answers, EvaluatedAt: time.Now().UTC()}
			evaluated++
		}
		rows = append(rows, row{pr: pr, verdict: verdict, answers: answers, note: label})
	}

	if err := saveState(statePath, state); err != nil {
		return fmt.Errorf("save state: %w", err)
	}

	report := renderReport(repo, rows, evaluated)
	if outPath != "" {
		if err := os.WriteFile(outPath, []byte(report), 0o644); err != nil {
			return fmt.Errorf("write report: %w", err)
		}
		fmt.Printf("wrote %s (%d PRs, %d new evaluations)\n", outPath, len(rows), evaluated)
		return nil
	}
	fmt.Print(report)
	return nil
}

func loadState(path string) (*stateFile, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return &stateFile{PRs: map[string]evalRecord{}}, nil
	}
	if err != nil {
		return nil, err
	}
	var state stateFile
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, err
	}
	if state.PRs == nil {
		state.PRs = map[string]evalRecord{}
	}
	return &state, nil
}

func saveState(path string, state *stateFile) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}

func renderReport(repo string, rows []row, evaluated int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Jev advisory tally - %s\n\n", repo)
	fmt.Fprintf(&b, "Measured %d merged PRs (%d fresh gateway evaluations, the rest cached). Ground truth is the pipeline's own recorded outcome: positive = the no-mistakes summary shows real findings or a fix round; negative = attested with none. Uncertain verdicts count as neither FP nor FN. PRs whose pipeline fixes landed before merge are labelled `positive (auto-fixed)`: the evaluated diff is the clean merged one, so a `clear` there reads the final state, not a miss, and they are excluded from the recall denominator.\n\n", len(rows), evaluated)

	fp, fn, tp, tn := 0, 0, 0, 0
	var flaggedOnNegative, clearOnPositive []string
	b.WriteString("| PR | ground truth | jev verdict | flagged questions |\n|---|---|---|---|\n")
	for _, r := range rows {
		var flagged []string
		for name, p := range r.answers {
			if p >= 0.6 {
				flagged = append(flagged, fmt.Sprintf("%s p=%.2f", name, p))
			}
		}
		sort.Strings(flagged)
		verdict := r.verdict
		if verdict == "" {
			verdict = "-"
		}
		fmt.Fprintf(&b, "| #%d %s | %s | %s | %s |\n", r.pr.Number, strings.ReplaceAll(r.pr.Title, "|", "\\|"), r.note, verdict, strings.Join(flagged, ", "))

		if r.verdict == "" || strings.HasPrefix(r.note, "positive (auto-fixed)") {
			continue
		}
		switch {
		case r.note == "negative" && r.verdict == "flagged":
			fp++
			flaggedOnNegative = append(flaggedOnNegative, fmt.Sprintf("#%d", r.pr.Number))
		case r.note == "negative" && r.verdict == "clear":
			tn++
		case r.note == "positive" && r.verdict == "clear":
			fn++
			clearOnPositive = append(clearOnPositive, fmt.Sprintf("#%d", r.pr.Number))
		case r.note == "positive" && r.verdict == "flagged":
			tp++
		}
	}
	precision := float64(tp)
	if tp+fp > 0 {
		precision = float64(tp) / float64(tp+fp)
	}
	recall := float64(tp)
	if tp+fn > 0 {
		recall = float64(tp) / float64(tp+fn)
	}
	fmt.Fprintf(&b, "\n**False positives (flagged, pipeline found nothing): %d** %s\n", fp, strings.Join(flaggedOnNegative, ", "))
	fmt.Fprintf(&b, "\n**False negatives (clear, pipeline found something that stayed unfixed at merge): %d** %s\n", fn, strings.Join(clearOnPositive, ", "))
	fmt.Fprintf(&b, "\nflagged-and-deserved %d, clear-and-clean %d, precision %.2f, recall %.2f (uncertain and auto-fixed excluded)\n", tp, tn, precision, recall)
	return b.String()
}

// transient reports whether an evaluation error looks like a service blip
// worth one retry (the harness's own policy; the pipeline step never retries).
func transient(errMsg string) bool {
	return strings.Contains(errMsg, "503") || strings.Contains(errMsg, "502") ||
		strings.Contains(errMsg, "504") || strings.Contains(errMsg, "unreachable") ||
		strings.Contains(strings.ToLower(errMsg), "timeout")
}
