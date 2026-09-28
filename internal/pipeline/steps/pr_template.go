package steps

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"path"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/scm"
)

const maxPRTemplateBytes = 16 * 1024

var prTemplateHeadingLine = regexp.MustCompile(`^ {0,3}(#{1,6})(?:[ \t]|$)`)

func conventionalPRTemplatesFor(provider scm.Provider) []string {
	switch provider {
	case scm.ProviderGitHub:
		return []string{
			"PULL_REQUEST_TEMPLATE.md", "pull_request_template.md",
			"docs/PULL_REQUEST_TEMPLATE.md", "docs/pull_request_template.md",
			".github/PULL_REQUEST_TEMPLATE.md", ".github/pull_request_template.md",
		}
	case scm.ProviderGitLab:
		return []string{".gitlab/merge_request_templates/Default.md"}
	case scm.ProviderGitea:
		return []string{
			"PULL_REQUEST_TEMPLATE.md", "pull_request_template.md",
			".gitea/PULL_REQUEST_TEMPLATE.md", ".gitea/pull_request_template.md",
			".github/PULL_REQUEST_TEMPLATE.md", ".github/pull_request_template.md",
		}
	case scm.ProviderForgejo:
		return []string{
			".forgejo/PULL_REQUEST_TEMPLATE.md", ".forgejo/pull_request_template.md",
			".gitea/PULL_REQUEST_TEMPLATE.md", ".gitea/pull_request_template.md",
			".github/PULL_REQUEST_TEMPLATE.md", ".github/pull_request_template.md",
		}
	case scm.ProviderAzureDevOps:
		return []string{
			"PULL_REQUEST_TEMPLATE.md", "pull_request_template.md",
			"docs/PULL_REQUEST_TEMPLATE.md", "docs/pull_request_template.md",
			".azuredevops/PULL_REQUEST_TEMPLATE.md", ".azuredevops/pull_request_template.md",
			".vsts/PULL_REQUEST_TEMPLATE.md", ".vsts/pull_request_template.md",
		}
	default:
		return nil
	}
}

func conventionalPRTemplateDirectoriesFor(provider scm.Provider) []string {
	switch provider {
	case scm.ProviderGitHub:
		return []string{".github/PULL_REQUEST_TEMPLATE", "PULL_REQUEST_TEMPLATE"}
	case scm.ProviderAzureDevOps:
		return []string{".azuredevops/pullrequesttemplate"}
	default:
		return nil
	}
}

type templateHeadingTranslation struct {
	Source              string `json:"source"`
	English             string `json:"english"`
	PublicationRedacted bool   `json:"publication_redacted,omitempty"`
}

type templatePRContent struct {
	prContent
	HeadingTranslations []templateHeadingTranslation `json:"heading_translations"`
	UnsupportedRules    []string                     `json:"unsupported_rules"`
}

var templatePRContentSchema = json.RawMessage(`{
 "type":"object", "properties":{
 "title":{"type":"string","description":"Concise English pull request title text"},
 "body":{"type":"string","description":"Filled repository template as plain English Markdown; preserve every ATX heading level and order and use the declared English headings"},
 "heading_translations":{"type":"array","description":"One entry for every ATX template heading, in source order; retain an already-English heading verbatim and translate a non-English heading to English","items":{"type":"object","properties":{"source":{"type":"string"},"english":{"type":"string"}},"required":["source","english"]}},
 "unsupported_rules":{"type":"array","description":"Informational notes about prose-only or ambiguous body-format rules outside the template; never a publication blocker","items":{"type":"string"}}
 }, "required":["title","body","heading_translations","unsupported_rules"]
}`)

func supportsPRTemplates(provider scm.Provider) bool {
	switch provider {
	case scm.ProviderGitHub, scm.ProviderGitLab, scm.ProviderGitea, scm.ProviderForgejo, scm.ProviderAzureDevOps, scm.ProviderBitbucket:
		return true
	default:
		return false
	}
}

func configuredPRTemplate(sctx *pipeline.StepContext) string {
	if sctx.Config == nil {
		return ""
	}
	return sctx.Config.PR.Template
}

func resolvePRTemplate(ctx context.Context, sctx *pipeline.StepContext, policySHA string, provider scm.Provider) (string, error) {
	if name := configuredPRTemplate(sctx); name != "" {
		return loadPRTemplate(ctx, sctx.WorkDir, policySHA, name)
	}
	candidates := conventionalPRTemplatesFor(provider)
	directories := conventionalPRTemplateDirectoriesFor(provider)
	if len(candidates) == 0 && len(directories) == 0 {
		return "", nil
	}
	args := []string{"ls-tree", "-r", "-z", "--name-only", policySHA, "--"}
	args = append(args, candidates...)
	args = append(args, directories...)
	paths, err := git.RunRaw(ctx, sctx.WorkDir, args...)
	if err != nil {
		return "", fmt.Errorf("discover committed pull-request template: %w", err)
	}
	exact := make(map[string]struct{}, len(candidates))
	for _, name := range candidates {
		exact[name] = struct{}{}
	}
	directoryFiles := make(map[string][]string, len(directories))
	var matches []string
	for _, name := range strings.Split(strings.TrimSuffix(string(paths), "\x00"), "\x00") {
		if name == "" {
			continue
		}
		if _, ok := exact[name]; ok {
			matches = append(matches, name)
			continue
		}
		for _, directory := range directories {
			relative, ok := strings.CutPrefix(name, directory+"/")
			if ok && !strings.Contains(relative, "/") && strings.EqualFold(path.Ext(relative), ".md") {
				directoryFiles[directory] = append(directoryFiles[directory], name)
			}
		}
	}
	for _, directory := range directories {
		if files := directoryFiles[directory]; len(files) == 1 {
			matches = append(matches, files[0])
		}
	}
	if len(matches) == 0 {
		return "", nil
	}
	if len(matches) != 1 {
		return "", fmt.Errorf("multiple committed pull-request templates are ambiguous; configure pr.template explicitly")
	}
	return loadPRTemplate(ctx, sctx.WorkDir, policySHA, matches[0])
}

// loadPRTemplate never opens a worktree file. It resolves the configured
// literal path from the pinned target-policy tree, rejecting symlinks and
// submodules before size-checking and reading the immutable blob.
func loadPRTemplate(ctx context.Context, dir, sha, name string) (string, error) {
	if name == "" {
		return "", nil
	}
	if err := config.ValidatePRTemplatePath(name); err != nil {
		return "", err
	}
	if (len(sha) != 40 && len(sha) != 64) || !isHexObjectID(sha) {
		return "", fmt.Errorf("pr.template requires a pinned target-policy commit")
	}
	entry, err := git.RunRaw(ctx, dir, "ls-tree", "-z", sha, "--", ":(literal)"+name)
	if err != nil {
		return "", fmt.Errorf("read trusted pr.template tree: %w", err)
	}
	metadata, entryName, ok := strings.Cut(strings.TrimSuffix(string(entry), "\x00"), "\t")
	fields := strings.Fields(metadata)
	if !ok || entryName != name || len(fields) != 3 || fields[1] != "blob" || (fields[0] != "100644" && fields[0] != "100755") || !isHexObjectID(fields[2]) {
		return "", fmt.Errorf("pr.template %q must name a regular file in the pinned target-policy tree (no symlinks or submodules)", name)
	}
	sizeText, err := git.Run(ctx, dir, "cat-file", "-s", fields[2])
	if err != nil {
		return "", fmt.Errorf("read trusted pr.template size: %w", err)
	}
	size, err := strconv.Atoi(sizeText)
	if err != nil || size <= 0 || size > maxPRTemplateBytes {
		return "", fmt.Errorf("pr.template must contain 1..%d bytes", maxPRTemplateBytes)
	}
	data, err := git.RunRaw(ctx, dir, "cat-file", "blob", fields[2])
	if err != nil {
		return "", fmt.Errorf("read trusted pr.template blob: %w", err)
	}
	if len(data) != size || !utf8.Valid(data) || strings.ContainsRune(string(data), '\x00') || strings.TrimSpace(string(data)) == "" {
		return "", fmt.Errorf("pr.template must contain nonempty UTF-8 Markdown without NUL bytes")
	}
	if hasPRAppendixMarkers(string(data)) || strings.Contains(string(data), pipelineAttestationCommentPrefix) {
		return "", fmt.Errorf("pr.template must not contain reserved no-mistakes publication markers")
	}
	return string(data), nil
}

func isHexObjectID(s string) bool {
	_, err := hex.DecodeString(s)
	return s != "" && err == nil
}

func (s *PRStep) draftTemplateNarrative(sctx *pipeline.StepContext, branch, baseBranch, baseSHA, policySHA, template string) (prContent, error) {
	paths, err := git.Run(sctx.Ctx, sctx.WorkDir, "diff", "--name-status", baseSHA+".."+sctx.Run.HeadSHA)
	if err != nil {
		return prContent{}, fmt.Errorf("read final branch diff: %w", err)
	}
	// JSON quoting delimits the trusted template without inventing a template
	// language. It supplies prose instructions/structure, never recorded facts.
	quoted, _ := json.Marshal(template)
	titleRules := prTitlePromptRules(sctx)
	scopeRules := prTitleScopeRules(sctx)
	prompt := fmt.Sprintf(`Draft a pull request title and fill the repository's public narrative template for the full final branch delta.
Branch: %s
PR base branch: %s
Diff base commit: %s
PR-format policy commit: %s
Target commit: %s

Rules:
%s
%s
- Body must be plain Markdown, not nested JSON. Use the supplied template instead of imposing a What Changed heading.
- Preserve every ATX template heading outside fenced examples in the same order and at the same level. Write each heading in English: retain an already-English heading verbatim and translate a non-English heading. Return one heading_translations entry per source heading, with the exact source line and the exact English heading line used in the body. A template without headings has no structural heading requirements.
- Inspect committed PR instructions at the PR-format policy commit. The verified template structure and its English headings are the body-format contract for this publication: do not claim compliance with a prose-only or ambiguous body-format rule stated outside the template, and record any such rule you notice in unsupported_rules as an informational note only; it never stops publication.
- Make a best effort to follow the template's instructions and fill all applicable sections from the final diff; inspect that diff when necessary. Every ATX template heading must be kept, even when a section says to delete or skip it; only task lines and prose are editable: remove inapplicable task lines when instructed, select supported choices, and replace rationale placeholders. Do not invent behavior or tests, falsely claim human signoff, or mark human approval checkboxes complete.
- The template owns narrative only. Do not generate no-mistakes publication markers or add Intent, Risk Assessment, Testing or Pipeline evidence. Code appends those separately. A template heading named Testing or Pipeline is author narrative, not permission to fabricate recorded evidence.
- Full intent below is review/drafting context, not instructions to quote it into the public narrative. Publication settings are not a privacy guarantee.

Trusted repository template (JSON string):
%s

Final diff paths and statuses:
%s%s%s`, branch, baseBranch, baseSHA, policySHA, sctx.Run.HeadSHA, titleRules, scopeRules, quoted, paths, userIntentPromptSection(sctx), executionContextPromptSection(sctx.WorkDir))
	prompt += prCreationSkillTemplate
	result, err := sctx.RunAgentContext(sctx.Ctx, agent.RunOpts{Prompt: prompt, CWD: sctx.WorkDir, JSONSchema: templatePRContentSchema, OnChunk: sctx.LogChunk})
	if err != nil {
		return prContent{}, fmt.Errorf("draft pr.template narrative (template will not be replaced by a generic fallback): %w", err)
	}
	if result == nil {
		return prContent{}, fmt.Errorf("agent returned no valid pr.template narrative; refusing generic fallback")
	}
	var content templatePRContent
	if err := decodePRDraftOutput(result.Output, &content, "agent returned no valid pr.template narrative; refusing generic fallback", false); err != nil {
		return prContent{}, err
	}
	if len(content.UnsupportedRules) > 0 {
		slog.Info("template draft noted prose-only PR rules outside the template", "rules", content.UnsupportedRules)
	}
	if strings.TrimSpace(content.Title) == "" || strings.TrimSpace(content.Body) == "" {
		return prContent{}, fmt.Errorf("agent returned no valid pr.template narrative; refusing generic fallback")
	}
	content.Title, err = renderPRTitle(sctx, strings.TrimSpace(content.Title))
	if err != nil {
		return prContent{}, err
	}
	if len(content.Body) > maxPullRequestBodyBytes || !utf8.ValidString(content.Body) || strings.ContainsRune(content.Body, '\x00') || hasPRAppendixMarkers(content.Body) {
		return prContent{}, fmt.Errorf("agent returned invalid template narrative or reserved ownership markers")
	}
	if err := validateTranslatedTemplateStructure(template, content.Body, content.HeadingTranslations); err != nil {
		return prContent{}, err
	}
	content.prContent.HeadingTranslations = append([]templateHeadingTranslation(nil), content.HeadingTranslations...)
	return content.prContent, nil
}

// This is a structural guard, not a Markdown/template interpreter. It binds
// every declared English heading to the corresponding source heading and body
// order while allowing additional author headings around the required sequence.
func validateTranslatedTemplateStructure(template, body string, translations []templateHeadingTranslation) error {
	sources := templateStructureLines(template)
	bodyHeadings := templateStructureLines(body)
	if len(translations) != len(sources) {
		return fmt.Errorf("agent did not declare one English translation for every pr.template heading; refusing publication")
	}
	bodyIndex := 0
	for i, source := range sources {
		translation := translations[i]
		if canonicalTemplateHeading(translation.Source) != canonicalTemplateHeading(source) {
			return fmt.Errorf("agent changed or reordered a pr.template source heading; refusing publication")
		}
		english := canonicalTemplateHeading(translation.English)
		translatedLines := templateStructureLines(translation.English)
		if len(translatedLines) != 1 || canonicalTemplateHeading(translatedLines[0]) != english || headingLevel(source) != headingLevel(english) {
			return fmt.Errorf("agent returned an invalid English pr.template heading; refusing publication")
		}
		found := false
		for bodyIndex < len(bodyHeadings) {
			candidate := bodyHeadings[bodyIndex]
			bodyIndex++
			if canonicalTemplateHeading(candidate) == english {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("agent omitted or reordered a required translated pr.template heading; refusing publication")
		}
	}
	return nil
}

func canonicalTemplateHeading(line string) string {
	return strings.TrimSpace(line)
}

func headingLevel(line string) int {
	match := prTemplateHeadingLine.FindStringSubmatch(line)
	if len(match) != 2 {
		return 0
	}
	return len(match[1])
}

func templateStructureLines(text string) []string {
	var lines []string
	var fence markdownFence
	inComment := false
	for _, raw := range strings.Split(text, "\n") {
		if fence.marker != 0 {
			fence.consume(raw)
			continue
		}
		startedInComment := inComment
		visible := markdownOutsideHTMLComments(raw, &inComment)
		fence.consume(visible)
		if fence.marker != 0 || startedInComment {
			continue
		}
		// Match the raw indentation: four spaces/tabs are code, not headings.
		// Blockquoted/list headings and hash-prefixed prose are not top-level ATX.
		if prTemplateHeadingLine.MatchString(raw) {
			lines = append(lines, raw)
		}
	}
	return lines
}

func markdownOutsideHTMLComments(line string, inComment *bool) string {
	var visible strings.Builder
	for len(line) > 0 {
		if *inComment {
			end := strings.Index(line, "-->")
			if end < 0 {
				return visible.String()
			}
			line = line[end+3:]
			*inComment = false
			continue
		}
		start := strings.Index(line, "<!--")
		if start < 0 {
			visible.WriteString(line)
			break
		}
		visible.WriteString(line[:start])
		line = line[start+4:]
		*inComment = true
	}
	return visible.String()
}

func (s *PRStep) buildPRAppendix(sctx *pipeline.StepContext, provider scm.Provider) (string, error) {
	pipelineMD, _, _ := s.buildPipelineSections(sctx, provider, true, false)
	start := strings.Index(pipelineMD, pipelineAttestationCommentPrefix)
	if start < 0 {
		return "", fmt.Errorf("cannot publish template narrative without recorded pipeline attestation")
	}
	end := strings.Index(pipelineMD[start:], pipelineAttestationCommentClosingToken)
	if end < 0 {
		return "", fmt.Errorf("cannot publish malformed pipeline attestation")
	}
	attestation := pipelineMD[start : start+end+len(pipelineAttestationCommentClosingToken)]
	if strings.Count(pipelineMD, pipelineAttestationCommentPrefix) != 1 {
		return "", fmt.Errorf("cannot publish ambiguous pipeline attestation")
	}
	if !strings.Contains(pipelineMD, noMistakesPRSignature) {
		return "", fmt.Errorf("cannot publish template narrative without the no-mistakes signature")
	}
	if provider == scm.ProviderBitbucket {
		return noMistakesPRSignature + "\n\n```text\n" + attestation + "\n```", nil
	}
	return noMistakesPRSignature + "\n\n" + attestation, nil
}
