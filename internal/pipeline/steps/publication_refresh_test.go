package steps

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

type refreshTestHost struct {
	attestationTestHost
	pr          *scm.PR
	checks      []scm.Check
	checkErr    error
	identityErr error
	onChecks    func()
}

func (h *refreshTestHost) ValidateExistingPRIdentity(context.Context, string, string, string) (*scm.PR, error) {
	return h.pr, h.identityErr
}
func (h *refreshTestHost) GetChecks(context.Context, *scm.PR) ([]scm.Check, error) {
	if h.onChecks != nil {
		h.onChecks()
	}
	return h.checks, h.checkErr
}

func refreshFixture(t *testing.T, owned bool) (*pipeline.StepContext, *refreshTestHost, ipc.RefreshPublicationParams) {
	t.Helper()
	f := newCIRepairFixture(t, false, nil)
	s := f.sctx
	p := ipc.RefreshPublicationParams{RepoID: s.Repo.ID, RunID: s.Run.ID, HeadSHA: f.headSHA, PRURL: *s.Run.PRURL}
	for _, err := range []error{s.DB.UpdateRunStatus(p.RunID, types.RunRunning), s.DB.UpdateRunPRURL(p.RunID, p.PRURL)} {
		if err != nil {
			t.Fatal(err)
		}
	}
	ci, err := s.DB.InsertStepResult(p.RunID, types.StepCI)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.DB.UpdateStepStatus(ci.ID, types.StepStatusRunning); err != nil {
		t.Fatal(err)
	}
	summary := "agent reported repair"
	if _, err := s.DB.InsertStepRoundWithRepair(ci.ID, 2, "auto_fix", nil, &summary, true, 42); err != nil {
		t.Fatal(err)
	}
	review, err := s.DB.InsertStepResult(p.RunID, types.StepReview)
	if err != nil {
		t.Fatal(err)
	}
	findings := `{"summary":"R1/R2/R3 require evidence","risk_level":"high","items":[{"id":"R1","severity":"error","description":"source concern remains unproven","action":"ask-user"}]}`
	if err := s.DB.SetStepFindings(review.ID, findings); err != nil {
		t.Fatal(err)
	}
	if err := s.DB.CompleteSkippedStep(review.ID, 0, 99, "", "historical skip"); err != nil {
		t.Fatal(err)
	}
	test, err := s.DB.InsertStepResult(p.RunID, types.StepTest)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.DB.FailStep(test.ID, "historical timeout", 100); err != nil {
		t.Fatal(err)
	}
	body := "Original high risk: R1/R2/R3 unproven. Test timed out.\n" + compliantPipelineBody(t, p.HeadSHA)
	if owned {
		body = "Author narrative\n" + wrapPRAppendix(body) + "\nAuthor suffix"
	}
	h := &refreshTestHost{attestationTestHost: attestationTestHost{body: body}, pr: &scm.PR{URL: p.PRURL, Number: "42", HeadSHA: p.HeadSHA, BaseBranch: "main"}, checks: []scm.Check{{Name: "test", Bucket: scm.CheckBucketPass}}}
	return s, h, p
}

func TestRefreshPublication_PreservesHistoricalEvidence(t *testing.T) {
	for _, owned := range []bool{false, true} {
		t.Run(map[bool]string{true: "owned", false: "legacy"}[owned], func(t *testing.T) {
			s, h, p := refreshFixture(t, owned)
			before, _ := s.DB.GetStepsByRun(p.RunID)
			runBefore, _ := s.DB.GetRun(p.RunID)
			original := h.body
			refsBefore := gitCmd(t, s.WorkDir, "ls-remote", "origin")
			roundsBefore := map[string][]*db.StepRound{}
			for _, row := range before {
				roundsBefore[row.ID], _ = s.DB.GetRoundsByStep(row.ID)
			}
			result, err := refreshRepairPublication(s, h, p)
			if err != nil {
				t.Fatal(err)
			}
			if result.HeadSHA != p.HeadSHA || h.updates != 1 || h.updated.Title != "" {
				t.Fatalf("result=%+v writes=%d", result, h.updates)
			}
			if !strings.Contains(h.updated.Body, "No finding is certified fixed by green CI") || !strings.Contains(h.updated.Body, "Recorded published CI repair round 2") {
				t.Fatal(h.updated.Body)
			}
			stripped, err := removeRepairDisposition(h.updated.Body)
			if owned {
				parts, parseErr := parsePROwnedBody(h.updated.Body)
				if parseErr != nil {
					t.Fatal(parseErr)
				}
				oldParts, _ := parsePROwnedBody(original)
				stripped, err = removeRepairDisposition(parts.appendix)
				if stripped != oldParts.appendix {
					t.Fatalf("historical appendix changed: %s", stripped)
				}
				if parts.before != oldParts.before || parts.after != oldParts.after {
					t.Fatal("author text changed")
				}
			} else if stripped != original {
				t.Fatal("historical prose changed")
			}
			if err != nil {
				t.Fatal(err)
			}
			after, _ := s.DB.GetStepsByRun(p.RunID)
			runAfter, _ := s.DB.GetRun(p.RunID)
			if refsAfter := gitCmd(t, s.WorkDir, "ls-remote", "origin"); refsAfter != refsBefore {
				t.Fatal("refresh pushed a remote ref")
			}
			for _, row := range after {
				roundsAfter, _ := s.DB.GetRoundsByStep(row.ID)
				if !reflect.DeepEqual(roundsBefore[row.ID], roundsAfter) {
					t.Fatal("refresh changed round history")
				}
			}
			if !reflect.DeepEqual(before, after) || !reflect.DeepEqual(runBefore, runAfter) {
				t.Fatal("refresh changed durable history")
			}
			h.body = h.updated.Body
			if _, err := refreshRepairPublication(s, h, p); err != nil {
				t.Fatal(err)
			}
			if strings.Count(h.updated.Body, repairDispositionStart) != 1 {
				t.Fatal("refresh duplicated section")
			}
		})
	}
}

func TestRefreshPublication_RefusesBindingsBeforeWrites(t *testing.T) {
	for _, name := range []string{"run", "repo", "pr", "short-head", "recorded", "pushed", "remote", "forge", "identity-error", "terminal", "parked", "head-moves", "edited-disposition", "attestation", "body-race", "repairing", "budget", "readerless"} {
		t.Run(name, func(t *testing.T) {
			s, h, p := refreshFixture(t, false)
			other := strings.Repeat("a", 40)
			switch name {
			case "run":
				p.RunID = "absent"
			case "repo":
				p.RepoID = "absent"
			case "pr":
				p.PRURL = "https://github.com/test/repo/pull/43"
			case "short-head":
				p.HeadSHA = p.HeadSHA[:8]
			case "recorded":
				s.DB.UpdateRunHeadSHA(p.RunID, other)
			case "pushed":
				s.DB.UpdateRunPushBinding(p.RunID, db.PushBinding{HeadSHA: other, TargetKind: "upstream", TargetFingerprint: "fixture", Ref: "refs/heads/feature"})
			case "remote":
				gitCmd(t, s.WorkDir, "push", "origin", ":feature")
			case "forge":
				h.pr.HeadSHA = other
			case "identity-error":
				h.identityErr = errors.New("unreadable")
			case "terminal":
				s.DB.UpdateRunStatus(p.RunID, types.RunFailed)
			case "parked":
				s.DB.SetRunAwaitingAgent(p.RunID)
			case "head-moves":
				h.onChecks = func() { s.DB.UpdateRunHeadSHA(p.RunID, other) }
			case "edited-disposition":
				h.body += "\n" + repairDispositionStart + "\nauthor note\n" + repairDispositionEnd
			case "attestation":
				h.body = strings.ReplaceAll(h.body, p.HeadSHA, other)
			case "body-race":
				h.bodyAfterFirstRead = h.body + "author concurrent edit"
			case "repairing":
				rows, _ := s.DB.GetStepsByRun(p.RunID)
				for _, row := range rows {
					if row.StepName == types.StepCI {
						s.DB.UpdateStepStatus(row.ID, types.StepStatusFixing)
					}
				}
			case "budget":
				h.body += strings.Repeat("x", maxPullRequestBodyBytes)
			case "readerless":
				if _, err := refreshRepairPublication(s, &readerlessHost{}, p); err == nil {
					t.Fatal("accepted readerless host")
				}
				return
			}
			if _, err := refreshRepairPublication(s, h, p); err == nil {
				t.Fatal("expected refusal")
			}
			if h.updates != 0 {
				t.Fatal("refusal wrote to forge")
			}
		})
	}
}

func TestRefreshPublication_InconclusiveEvidenceNeverPasses(t *testing.T) {
	for _, tc := range []struct {
		name   string
		checks []scm.Check
		err    error
		want   string
	}{
		{name: "unreadable", err: errors.New("unavailable"), want: "inconclusive"},
		{name: "empty", want: "inconclusive"},
		{name: "pending", checks: []scm.Check{{Bucket: scm.CheckBucketPending}}, want: "unresolved"},
		{name: "failed", checks: []scm.Check{{Bucket: scm.CheckBucketFail}}, want: "unresolved"},
		{name: "skip", checks: []scm.Check{{Bucket: scm.CheckBucketSkip}}, want: "unresolved"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, h, p := refreshFixture(t, false)
			h.checks = tc.checks
			h.checkErr = tc.err
			result, err := refreshRepairPublication(s, h, p)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.HasPrefix(result.Disposition, tc.want) || strings.Contains(result.Disposition, "checks passed") {
				t.Fatal(result.Disposition)
			}
		})
	}
}
