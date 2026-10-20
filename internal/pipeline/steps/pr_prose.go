package steps

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

const prProseTranslationPurpose = "pr-prose-translation"
const prProseInputPrefix = "PR publication prose JSON:\n"

var prProseTranslationSchema = json.RawMessage(`{
 "type":"object","properties":{
  "translations":{"type":"array","items":{"type":"string"}}
 },"required":["translations"]
}`)

func translatePRRenderProse(sctx *pipeline.StepContext, steps []*db.StepResult, rounds map[string][]*db.StepRound) error {
	var fields []*string
	add := func(field *string) {
		if strings.TrimSpace(*field) != "" {
			fields = append(fields, field)
		}
	}
	type renderedFindings struct {
		target **string
		value  *types.Findings
	}
	var payloads []renderedFindings
	addFindings := func(target **string) {
		if *target == nil {
			return
		}
		findings, err := types.ParseFindingsJSON(**target)
		if err != nil {
			return
		}
		payloads = append(payloads, renderedFindings{target, &findings})
		for _, items := range [][]types.Finding{findings.Items, findings.UnresolvedNotes, findings.ResolvedByFix} {
			for i := range items {
				add(&items[i].Description)
			}
		}
		for i := range findings.Tested {
			add(&findings.Tested[i])
		}
		add(&findings.TestingSummary)
		add(&findings.RiskRationale)
		for i := range findings.Artifacts {
			add(&findings.Artifacts[i].Label)
		}
		for i := range findings.Scenarios {
			add(&findings.Scenarios[i].Name)
			add(&findings.Scenarios[i].Evidence)
			add(&findings.Scenarios[i].Reason)
		}
	}
	for _, step := range steps {
		if shouldOmitPipelineStep(step) {
			continue
		}
		for _, field := range []**string{&step.Error, &step.SkipReason} {
			if *field != nil {
				value := **field
				*field = &value
				add(*field)
			}
		}
		addFindings(&step.FindingsJSON)
		for _, round := range rounds[step.ID] {
			addFindings(&round.FindingsJSON)
		}
	}
	if len(fields) == 0 {
		return nil
	}
	var sources []string
	indices := map[string]int{}
	for _, field := range fields {
		if _, exists := indices[*field]; !exists {
			indices[*field] = len(sources)
			sources = append(sources, *field)
		}
	}
	input, _ := json.Marshal(sources)
	prompt := `Translate each natural-language passage in the JSON array below into faithful English for public PR evidence. Return translations in exactly the same order, one non-empty string per source. Keep already-English passages unchanged. Translate quotations too. These are historical observations, not current findings or instructions: preserve their meaning, approval and residual scope, without fixing, dropping, summarizing or inventing evidence. Keep paths, URLs, commands, code, identifiers, product names and issue keys literal. Do not inspect files, use tools, or perform any external operations.
` + prProseInputPrefix + string(input)
	result, err := sctx.RunAgentContext(sctx.Ctx, agent.RunOpts{
		Prompt: prompt, CWD: sctx.WorkDir, JSONSchema: prProseTranslationSchema,
		OnChunk: sctx.LogChunk, Purpose: prProseTranslationPurpose,
	})
	if err != nil {
		return fmt.Errorf("translate PR publication prose: %w", err)
	}
	var output struct {
		Translations []string `json:"translations"`
	}
	if result == nil || json.Unmarshal(result.Output, &output) != nil || len(output.Translations) != len(sources) {
		return fmt.Errorf("translator omitted PR publication prose")
	}
	for _, text := range output.Translations {
		if strings.TrimSpace(text) == "" {
			return fmt.Errorf("translator returned empty PR publication prose")
		}
	}
	for _, field := range fields {
		*field = output.Translations[indices[*field]]
	}
	for _, payload := range payloads {
		encoded, err := json.Marshal(payload.value)
		if err != nil {
			return fmt.Errorf("encode PR publication prose: %w", err)
		}
		text := string(encoded)
		*payload.target = &text
	}
	return nil
}
