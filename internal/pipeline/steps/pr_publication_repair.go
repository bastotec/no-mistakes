package steps

import (
	"fmt"
	"os"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/kunchenguid/no-mistakes/internal/safepath"
)

// maxPRPublicationRepairs bounds how many times the PR step sanitizes and
// re-drafts run-written text after the publication validator refuses it.
const maxPRPublicationRepairs = 2

// Placeholders are never longer than the text they replace, so redaction can
// only shrink an already length-capped body. Square brackets carry no HTML
// meaning, unlike an angle-bracketed token that GitHub would drop as a tag.
const (
	runIDPlaceholder       = "[run id]"
	internalRefPlaceholder = "[internal ref]"
	localPathPlaceholder   = "[local path]"
	learnedPlaceholder     = "[redacted]"
)

// runIDPattern matches a ULID, the shape of every no-mistakes run, step and
// round id: 26 upper-case Crockford base32 characters whose first character
// is at most 7. Commit SHAs are lower-case hex and never match.
var runIDPattern = regexp.MustCompile(`\b[0-7][0-9A-HJKMNP-TV-Z]{25}\b`)

// internalRefPattern matches the private refs/no-mistakes/... namespace the
// daemon keeps for sync, recovery and survived work.
var internalRefPattern = regexp.MustCompile(`\brefs/no-mistakes/[^\s` + "`" + `'"<>()\[\]{},;]*`)

// privacyIssuePattern recognizes a validator issue about leaked internal or
// private data, as opposed to a fabrication or language complaint. Only those
// issues teach the repair literal identifiers to strip: redacting a token a
// fabrication issue quotes would hide the claim instead of fixing it.
var privacyIssuePattern = regexp.MustCompile(`(?i)internal|private|privacy|identifier|secret|credential|token|leak|expos|sensitive|confidential|personal|local path|daemon|refs/`)

var fabricationIssuePattern = regexp.MustCompile(`(?i)fabricat|invent|absent|unsupported[\s-]+claim`)

var issueLiteralPattern = regexp.MustCompile("`([^`]*)`")

// redactRunIdentifiers removes no-mistakes run ids and private refs from
// run-written publication text, including the run id segment of a daemon-local
// path.
func redactRunIdentifiers(text string) string {
	if text == "" {
		return text
	}
	if root := strings.TrimSpace(os.Getenv("NM_HOME")); len(root) >= 4 && (strings.HasPrefix(root, "/") || strings.Contains(root, `:\`)) {
		if strings.ReplaceAll(safepath.RedactText(root), `\`, "/") != "~/.no-mistakes" {
			text = strings.ReplaceAll(text, root, localPathPlaceholder)
		}
	}
	text = replaceOutsideURLs(text, internalRefPattern, internalRefPlaceholder)
	return replaceOutsideURLs(text, runIDPattern, runIDPlaceholder)
}

// machineMarkerPattern matches no-mistakes' own HTML-comment markers (the
// test-preview owner marker carries the run id on purpose). They are invisible
// in the rendered PR and other code reads them back, so they stay verbatim.
var machineMarkerPattern = regexp.MustCompile(`(?s)<!-- no-mistakes.*?-->`)

func replaceOutsideURLs(text string, pattern *regexp.Regexp, placeholder string) string {
	matches := pattern.FindAllStringIndex(text, -1)
	if len(matches) == 0 {
		return text
	}
	markers := machineMarkerPattern.FindAllStringIndex(text, -1)
	insideMarker := func(i int) bool {
		for _, m := range markers {
			if i >= m[0] && i < m[1] {
				return true
			}
		}
		return false
	}
	var b strings.Builder
	last := 0
	for _, m := range matches {
		if insidePublishedURL(text, m[0]) || insideMarker(m[0]) {
			continue
		}
		b.WriteString(text[last:m[0]])
		b.WriteString(placeholder)
		last = m[1]
	}
	b.WriteString(text[last:])
	return b.String()
}

func insidePublishedURL(text string, i int) bool {
	prefix := text[:i]
	scheme := strings.LastIndex(prefix, "://")
	return scheme >= 0 && strings.IndexFunc(prefix[scheme:], func(r rune) bool {
		return unicode.IsSpace(r) || strings.ContainsRune("\"'`<>()[]{},", r)
	}) < 0
}

// prPublicationRefusal is the validator's verdict that run-written PR text is
// not publishable. Nothing has been written to the forge when it is returned.
type prPublicationRefusal struct {
	issues []string
	detail string
}

func (e *prPublicationRefusal) Error() string {
	return "refusing non-compliant PR publication: " + e.detail
}

// prPublicationRepair carries what earlier refusals in this PR step taught:
// literal identifiers to strip from run-written text and the issues the next
// draft must avoid. A nil repair is a first attempt.
type prPublicationRepair struct {
	issues   []string
	literals []string
}

// learn records a refusal's issues and reports how many new literals it
// taught.
func (r *prPublicationRepair) learn(issues []string) int {
	added := 0
	for _, issue := range issues {
		issue = strings.TrimSpace(issue)
		if issue == "" {
			continue
		}
		r.issues = append(r.issues, issue)
		classification := issueLiteralPattern.ReplaceAllString(issue, "")
		if !privacyIssuePattern.MatchString(classification) || fabricationIssuePattern.MatchString(classification) {
			continue
		}
		for _, match := range issueLiteralPattern.FindAllStringSubmatch(issue, -1) {
			literal := match[1]
			if utf8.RuneCountInString(literal) >= 3 && !containsString(r.literals, literal) {
				r.literals = append(r.literals, literal)
				added++
			}
		}
	}
	return added
}

func containsString(values []string, want string) bool {
	for _, v := range values {
		if v == want {
			return true
		}
	}
	return false
}

// scrub strips learned literals from run-written text. The machine-readable
// pipeline attestation comment is left byte for byte: consumers parse it.
func (r *prPublicationRepair) scrub(text string) string {
	if r == nil || len(r.literals) == 0 || text == "" {
		return text
	}
	var b strings.Builder
	for text != "" {
		start := strings.Index(text, pipelineAttestationCommentPrefix)
		if start < 0 {
			b.WriteString(r.scrubSpan(text))
			break
		}
		end := strings.Index(text[start:], pipelineAttestationCommentClosingToken)
		if end < 0 {
			b.WriteString(r.scrubSpan(text[:start]))
			b.WriteString(text[start:])
			break
		}
		end += start + len(pipelineAttestationCommentClosingToken)
		b.WriteString(r.scrubSpan(text[:start]))
		b.WriteString(text[start:end])
		text = text[end:]
	}
	return b.String()
}

func (r *prPublicationRepair) scrubSpan(text string) string {
	for _, literal := range r.literals {
		text = strings.ReplaceAll(text, literal, learnedPlaceholder)
	}
	return text
}

func (r *prPublicationRepair) scrubContent(content prContent) prContent {
	if r == nil || len(r.literals) == 0 {
		return content
	}
	content.Title = r.scrub(content.Title)
	content.Body = r.scrub(content.Body)
	content.HeadingTranslations = append([]templateHeadingTranslation(nil), content.HeadingTranslations...)
	for i := range content.HeadingTranslations {
		original := content.HeadingTranslations[i].English
		content.HeadingTranslations[i].English = r.scrub(original)
		if content.HeadingTranslations[i].English != original {
			content.HeadingTranslations[i].PublicationRedacted = true
		}
	}
	return content
}

// promptSection tells a re-draft why the previous one was refused.
func (r *prPublicationRepair) promptSection() string {
	if r == nil || len(r.issues) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\nPublication validator feedback:\n- An earlier draft of this run's pull-request text was refused before publication. Write this draft so none of these issues recur. Never quote no-mistakes run ids, refs/no-mistakes refs, or daemon-local paths:\n")
	for _, issue := range r.issues {
		b.WriteString("  - ")
		b.WriteString(sanitizePromptText(issue))
		b.WriteString("\n")
	}
	return b.String()
}

// exhaustedPRPublicationError is the terminal PR step failure after every
// sanitize-and-retry attempt was refused. It always names a next step.
func exhaustedPRPublicationError(refusal *prPublicationRefusal, branch, baseBranch string) error {
	return fmt.Errorf("%w (still refused after %d sanitize-and-retry attempts; nothing was published and the pushed branch is intact). Next: run `no-mistakes rerun` on branch %s to re-run the pipeline and re-draft the PR text, or open the pull request by hand, e.g. `gh pr create --head %s --base %s`", refusal, maxPRPublicationRepairs, branch, branch, baseBranch)
}
