package steps

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
)

func TestPRTemplateStructureRequiresHeadingLevelsAndOrder(t *testing.T) {
	t.Parallel()
	template := "# Summary\n## Details\n# Validation\n"
	for name, body := range map[string]string{
		"missing":          "# Summary\nDetails",
		"changed":          "# Summary\n# Tests",
		"wrong identities": "# Overview\n## Rollout\n# Validation",
		"reordered":        "# Validation\n# Summary",
		"demoted":          "## Summary\n# Validation",
		"fenced":           "```markdown\n# Summary\n# Validation\n```",
	} {
		t.Run(name, func(t *testing.T) {
			if err := validateTranslatedTemplateStructure(template, body, identityHeadingTranslations(template)); err == nil {
				t.Fatal("invalid H1 structure accepted")
			}
		})
	}
	body := "# Summary\nFilled.\n## Reviewer Notes\nAuthor addition.\n## Details\n# Validation\nDone."
	if err := validateTranslatedTemplateStructure(template, body, identityHeadingTranslations(template)); err != nil {
		t.Fatalf("additional author heading rejected: %v", err)
	}
}

func TestPRTemplateStructureRecognizesATXHeadings(t *testing.T) {
	t.Parallel()
	text := "# One\n  # Two ###\n#\n#\tTabbed\n## Two hashes\n### Three hashes\n#hashtag\n    # Indented code\n\t# Tab code\n> # Quoted\n- # List\nSetext\n======\n```markdown\n# Fenced\n```\n~~~\n# Tilde fenced\n~~~\n"
	want := []string{"# One", "# Two ###", "#", "#\tTabbed", "## Two hashes", "### Three hashes"}
	if got := templateStructureLines(text); !reflect.DeepEqual(got, want) {
		t.Fatalf("H1s = %q, want %q", got, want)
	}
	if err := validateTranslatedTemplateStructure("## Optional\n### Nested\n- [ ] Choice\n```\n# Example\n```", "Narrative only.", identityHeadingTranslations("## Optional\n### Nested")); err == nil {
		t.Fatal("required lower-level headings were omitted")
	}
}

// A faithful agent echoes the template heading exactly as written, whitespace
// included, so the guard must compare the trimmed forms it derived its own
// structure lines from - not demand the agent normalize them.
func TestPRTemplateStructureAcceptsVerbatimWhitespaceHeadings(t *testing.T) {
	t.Parallel()
	for name, template := range map[string]string{
		"trailing space": "# Overview \n\nDescribe the change.\n",
		"leading spaces": "   ## Summary\n\nDescribe the change.\n",
	} {
		t.Run(name, func(t *testing.T) {
			var translations []templateHeadingTranslation
			var bodyLines []string
			for _, raw := range strings.Split(template, "\n") {
				if !prTemplateHeadingLine.MatchString(raw) {
					continue
				}
				translations = append(translations, templateHeadingTranslation{Source: raw, English: raw})
				bodyLines = append(bodyLines, raw)
			}
			body := strings.Join(bodyLines, "\n") + "\n\nFilled narrative.\n"
			if err := validateTranslatedTemplateStructure(template, body, translations); err != nil {
				t.Fatalf("verbatim whitespace heading refused: %v", err)
			}
		})
	}
}

// Representative abbreviated shapes from the pinned Chatwoot, Forem and
// OpenProject compatibility report; fake drafts test acceptance, not model quality.
func TestPRTemplateDraftAllowsSubordinateCompletion(t *testing.T) {
	t.Parallel()
	cases := []struct{ name, template, body string }{
		{"Chatwoot", "# Pull Request Template\n## Description\n## Type of change\nPlease delete options that are not relevant.\n- [ ] Bug fix\n- [ ] New feature\n## Checklist:\n- [ ] Maintainer approval",
			"# Pull Request Template\n## Description\nFix the helper.\n## Type of change\n- [x] Bug fix\n## Checklist:\n- [ ] Maintainer approval"},
		{"Forem", "## What type of PR is this?\n- [ ] Bug Fix\n## Added/updated tests?\n- [ ] Yes\n- [ ] No, and this is why: _please replace this line with details on why tests\n      have not been included_\n### UI accessibility checklist\n- [ ] Keyboard operation\n## [optional] GIF",
			"## What type of PR is this?\n- [x] Bug Fix\n## Added/updated tests?\n- [x] No, and this is why: this is a prose-only correction.\n### UI accessibility checklist\nNot applicable to this prose-only change.\n## [optional] GIF\nNot applicable."},
		{"OpenProject", "# Ticket\n# What are you trying to accomplish?\n## Screenshots\n<!-- Provide before/after screenshots for visual changes; otherwise, remove this section -->\n# What approach did you choose and why?\n# Merge checklist\n- [ ] Tested major browsers",
			"# Ticket\nNo ticket supplied.\n# What are you trying to accomplish?\nCorrect prose.\n## Screenshots\nNot applicable; there are no visual changes.\n# What approach did you choose and why?\nEdit only the incorrect sentence.\n# Merge checklist\n- [ ] Tested major browsers"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sctx, ag, _ := templateTestContext(t)
			ag.runFn = func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
				for _, rule := range []string{"every ATX template heading", "Make a best effort", "fill all applicable sections", "falsely claim human signoff", "mark human approval checkboxes complete"} {
					if !strings.Contains(opts.Prompt, rule) {
						t.Errorf("missing drafting rule %q", rule)
					}
				}
				data, _ := json.Marshal(templateDraft("fix: correct narrative", tc.body, tc.template))
				return &agent.Result{Output: data}, nil
			}
			got, err := (&PRStep{}).draftTemplateNarrative(sctx, "feature", "main", sctx.Run.BaseSHA, sctx.Run.BaseSHA, tc.template)
			if err != nil || got.Body != tc.body {
				t.Fatalf("draft = %+v, error %v", got, err)
			}
		})
	}
}
