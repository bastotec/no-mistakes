package steps

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
)

const prContentValidationPurpose = "pr-content-validation"

const prCreationSkill = `
Shared pull-request creation skill:
- Inspect only the pinned PR-format policy revision named in the task for repository pull-request rules.
- A committed Markdown pull-request template is the only mechanically enforceable custom body format. Preserve every ATX heading level and order, translating heading text to English when necessary.
- Report prose-only or ambiguous body-format rules as unsupported instead of claiming compliance. Never replace them with the default format.
- Write all natural-language title and body text in English. Paths, URLs, commands, code, identifiers, product names, and repository issue keys remain literal.
- Do not invent validation, sign-off, behavior, or evidence.
`

var prContentValidationSchema = json.RawMessage(`{
 "type":"object","properties":{
  "content_sha256":{"type":"string","pattern":"^[0-9a-f]{64}$"},
  "english":{"type":"boolean"},
  "format_compliant":{"type":"boolean"},
  "heading_translations":{"type":"array","items":{"type":"object","properties":{"source":{"type":"string"},"english":{"type":"string"}},"required":["source","english"]}},
  "issues":{"type":"array","items":{"type":"string"}}
 },"required":["content_sha256","english","format_compliant","heading_translations","issues"]
}`)

type prContentValidation struct {
	ContentSHA256       string                       `json:"content_sha256"`
	English             bool                         `json:"english"`
	FormatCompliant     bool                         `json:"format_compliant"`
	HeadingTranslations []templateHeadingTranslation `json:"heading_translations"`
	Issues              []string                     `json:"issues"`
}

func defaultSectionsWithoutIntent(body string, sctx *pipeline.StepContext) string {
	cleaned := neutralizeAttestationMarkers(publicPRIntent(sctx))
	section := "## Intent\n\n" + cleaned
	if cleaned == "" || !strings.HasPrefix(body, section) {
		return body
	}
	return strings.TrimLeft(strings.TrimPrefix(body, section), "\n")
}

func validateDefaultPRHeadingOrder(body string) error {
	order := map[string]int{"Intent": 0, "What Changed": 1, "Risk Assessment": 2, "Testing": 3, "Pipeline": 4}
	seen := map[string]bool{}
	last := -1
	for _, line := range templateStructureLines(body) {
		if headingLevel(line) != 2 {
			continue
		}
		name := strings.TrimSpace(strings.TrimRight(strings.TrimSpace(strings.TrimPrefix(line, "##")), "#"))
		position, ok := order[name]
		if !ok {
			continue
		}
		if seen[name] || position < last {
			return fmt.Errorf("final PR body does not preserve the supported section order")
		}
		seen[name] = true
		last = position
	}
	if !seen["What Changed"] {
		return fmt.Errorf("final PR body is missing the required What Changed section")
	}
	return nil
}

func prContentDigest(content prContent) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(content.Title+"\x00"+content.Body)))
}

func (s *PRStep) validateFinalPRContent(sctx *pipeline.StepContext, content, runWritten prContent, template, baseSHA string, defaultFormat bool) error {
	if strings.TrimSpace(content.Title) == "" || strings.TrimSpace(content.Body) == "" {
		return fmt.Errorf("cannot validate empty pull-request title or body")
	}
	if template == "" && defaultFormat {
		if err := validateDefaultPRHeadingOrder(defaultSectionsWithoutIntent(content.Body, sctx)); err != nil {
			return err
		}
	}
	digest := prContentDigest(runWritten)
	contentJSON, _ := json.Marshal(runWritten)
	templateJSON, _ := json.Marshal(template)
	headingSourcesJSON, _ := json.Marshal(templateStructureLines(template))
	prompt := fmt.Sprintf(`Validate the pull-request title and body written by this run, below, before publication. Text owned by the pull request's author is preserved separately and is out of scope.
%s
The SHA-256 binds your verdict to the exact title and body below. Return it unchanged.
- pr_format_policy_sha: %s
- content_sha256: %s
- required_heading_sources_json: %s
- english is true only when every natural-language passage below is English. Ignore literal paths, URLs, commands, code, identifiers, product names, repository issue keys, and machine metadata.
- An empty title below means this run wrote no title; there is nothing to judge there.
- format_compliant is true only when the body below follows the supplied committed Markdown template, when non-empty, and contains its required sections in order. If no template is supplied, set it false when committed prose-only or ambiguous PR body rules make compliance mechanically unsupported.
- heading_translations contains one source/English pair for every required_heading_sources_json entry, in order. Copy source exactly; English must be the exact English heading present in the body below. Return an empty array when there is no template.
- List every concrete violation in issues. Do not edit or translate the content.

Committed template JSON string:
%s

Run-written PR content JSON:
%s`, prCreationSkill, baseSHA, digest, headingSourcesJSON, templateJSON, contentJSON)
	result, err := sctx.RunAgentContext(sctx.Ctx, agent.RunOpts{
		Prompt:     prompt,
		CWD:        sctx.WorkDir,
		JSONSchema: prContentValidationSchema,
		OnChunk:    sctx.LogChunk,
		Purpose:    prContentValidationPurpose,
	})
	if err != nil {
		return fmt.Errorf("validate final PR content: %w", err)
	}
	var validation prContentValidation
	if result == nil || json.Unmarshal(result.Output, &validation) != nil {
		return fmt.Errorf("validator returned no valid final PR content verdict")
	}
	if validation.ContentSHA256 != digest {
		return fmt.Errorf("validator verdict does not bind the final PR content")
	}
	if !validation.English || !validation.FormatCompliant || len(validation.Issues) != 0 {
		detail := strings.Join(validation.Issues, "; ")
		if detail == "" {
			detail = "English or repository-format compliance was not proven"
		}
		return fmt.Errorf("refusing non-compliant PR publication: %s", detail)
	}
	if template != "" {
		if err := validateTranslatedTemplateStructure(template, content.Body, validation.HeadingTranslations); err != nil {
			return fmt.Errorf("validate final PR template headings: %w", err)
		}
	} else if len(validation.HeadingTranslations) != 0 {
		return fmt.Errorf("validator returned heading translations without a committed template")
	}
	return nil
}
