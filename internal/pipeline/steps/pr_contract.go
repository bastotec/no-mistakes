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
- Inspect only the committed base revision for repository pull-request rules.
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
  "issues":{"type":"array","items":{"type":"string"}}
 },"required":["content_sha256","english","format_compliant","issues"]
}`)

type prContentValidation struct {
	ContentSHA256   string   `json:"content_sha256"`
	English         bool     `json:"english"`
	FormatCompliant bool     `json:"format_compliant"`
	Issues          []string `json:"issues"`
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
		if !ok || seen[name] || position < last {
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

func (s *PRStep) validateFinalPRContent(sctx *pipeline.StepContext, content prContent, template, baseSHA string, defaultFormat bool) error {
	if strings.TrimSpace(content.Title) == "" || strings.TrimSpace(content.Body) == "" {
		return fmt.Errorf("cannot validate empty pull-request title or body")
	}
	if template != "" {
		if err := validateTemplateHeadingShape(template, content.Body); err != nil {
			return err
		}
	} else if defaultFormat {
		if err := validateDefaultPRHeadingOrder(content.Body); err != nil {
			return err
		}
	}
	digest := prContentDigest(content)
	contentJSON, _ := json.Marshal(content)
	templateJSON, _ := json.Marshal(template)
	prompt := fmt.Sprintf(`Validate the exact final pull-request title and body below before publication.
%s
The SHA-256 binds your verdict to the exact title and body. Return it unchanged.
- committed_base_sha: %s
- mechanical_default_format: %t
- content_sha256: %s
- english is true only when every natural-language passage is English. Ignore literal paths, URLs, commands, code, identifiers, product names, repository issue keys, and machine metadata.
- format_compliant is true only when the final body follows the supplied committed Markdown template, when non-empty, and contains its required sections in order. If no template is supplied, set it false when committed prose-only or ambiguous PR body rules make compliance mechanically unsupported.
- List every concrete violation in issues. Do not edit or translate the content.

Committed template JSON string:
%s

Final PR content JSON:
%s`, prCreationSkill, baseSHA, defaultFormat, digest, templateJSON, contentJSON)
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
	return nil
}
