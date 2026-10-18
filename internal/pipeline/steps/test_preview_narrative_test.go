package steps

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestPRStep_PreviewTransitionDraftsNarrativeWithoutDiscardedUploads(t *testing.T) {
	for _, template := range []bool{false, true} {
		name := "default"
		if template {
			name = "template"
		}
		t.Run(name, func(t *testing.T) {
			var s *pipeline.StepContext
			var base string
			if template {
				s, _, _ = templateTestContext(t)
				base = s.Run.BaseSHA
			} else {
				dir, b, head := setupGitRepo(t)
				base = b
				s = newTestContextWithDBRecords(t, prDraftAgent(), dir, base, head, config.Commands{})
			}
			enableDefaultEvidence(s)
			png := writeEvidenceFile(t, s.EvidenceDir, "checkout.png", []byte("png-bytes"))
			insertCompletedStep(t, s, types.StepTest, screenshotFindings(png), "")
			const url = "https://github.com/test/repo/pull/99"
			if err := s.DB.BindTestPreview(s.Run.ID, url); err != nil {
				t.Fatal(err)
			}
			if err := s.DB.RecordTestPreviewValidation(s.Run.ID, s.Run.HeadSHA); err != nil {
				t.Fatal(err)
			}
			summary, _, _ := (&PRStep{}).buildPipelineSections(s, scm.ProviderGitHub, false, false)
			preview, err := composeOwnedPRContent(prOwnedBody{}, "PR validation preview", testPreviewMarker(s.Run.ID)+"\n\nTest is still pending.\n\n"+summary, scm.MaxPRBodyChars(scm.ProviderGitHub))
			if err != nil {
				t.Fatal(err)
			}
			policy, err := resolvePRPolicySHA(s.Ctx, s, "main")
			if err != nil {
				t.Fatal(err)
			}
			resolvedBase, err := resolveBranchBaseSHA(s.Ctx, s, base, "main")
			if err != nil {
				t.Fatal(err)
			}
			// Reconstruct the former render-then-strip result and compare the exact
			// published bytes, including the appendix integrity digest. This is a
			// local fake-forge regression, not a hosted-preview readiness proof.
			oldUploader := &stubMediaUploader{t: t, urls: map[string]string{"checkout.png": testAttachmentURL}}
			step := &PRStep{mediaUploader: oldUploader}
			draft, err := step.buildPRContentWithPolicy(s, "feature", "main", resolvedBase, policy, scm.ProviderGitHub, scm.MaxPRBodyChars(scm.ProviderGitHub))
			if err != nil {
				t.Fatal(err)
			}
			narrative := stripGeneratedSections(draft.Body)
			if hasPRAppendixMarkers(draft.Body) {
				parts, err := parsePROwnedBody(draft.Body)
				if err != nil {
					t.Fatal(err)
				}
				narrative = parts.before + parts.after
			}
			appendix, err := step.buildPRAppendix(s, scm.ProviderGitHub)
			if err != nil {
				t.Fatal(err)
			}
			expected, err := composeOwnedPRContent(prOwnedBody{before: narrative}, draft.Title, appendix, scm.MaxPRBodyChars(scm.ProviderGitHub))
			if err != nil {
				t.Fatal(err)
			}
			// Ensure the artifact actually exercises the discarded-upload bug for
			// the ordinary writer; template composition never renders Testing.
			if !template && len(oldUploader.calls) != 1 {
				t.Fatalf("legacy uploads=%v", oldUploader.calls)
			}
			// Give the final publication an uncached fingerprint, so the legacy
			// render above cannot mask an unwanted upload through cache reuse.
			if err := os.WriteFile(png, []byte("fresh-png-bytes"), 0600); err != nil {
				t.Fatal(err)
			}
			body := filepath.Join(t.TempDir(), "body")
			if err := os.WriteFile(body, []byte(preview.Body), 0600); err != nil {
				t.Fatal(err)
			}
			bin := fakeCLIBinDir(t)
			linkTestBinary(t, bin, "gh")
			s.Env = fakeCLIEnv(bin, map[string]string{"FAKE_CLI_MODE": "test-preview-gh", "FAKE_CLI_PR_BODY_FILE": body, "FAKE_CLI_PR_HEAD_SHA": s.Run.HeadSHA})
			uploader := &stubMediaUploader{t: t, urls: map[string]string{"checkout.png": testAttachmentURL}}
			outcome, err := (&PRStep{mediaUploader: uploader}).Execute(s)
			if err != nil {
				t.Fatal(err)
			}
			if outcome != nil && outcome.Skipped {
				t.Fatalf("skipped publication: %+v", outcome)
			}
			if len(uploader.calls) != 0 {
				t.Fatalf("discarded evidence uploads=%v", uploader.calls)
			}
			actual, err := os.ReadFile(body)
			if err != nil {
				t.Fatal(err)
			}
			if string(actual) != expected.Body {
				t.Fatalf("publication bytes changed:\ngot: %q\nwant: %q", actual, expected.Body)
			}
			if strings.Contains(string(actual), "Test is still pending") {
				t.Fatal("pending preview narrative survived")
			}
		})
	}
}
