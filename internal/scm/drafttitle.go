package scm

import "strings"

// draftTitleMarkers is the single owner of the title prefixes a forge treats
// as draft state. GitLab re-applies one when writing an MR title, and the PR
// readback comparison strips them before comparing, so both sides must agree:
// a marker one side writes and the other does not strip would read as
// divergence forever after a write that actually landed.
var draftTitleMarkers = []string{"draft:", "[draft]", "(draft)"}

// HasDraftTitleMarker reports whether title carries a leading draft marker
// ("Draft:", "[Draft]", or "(Draft)"), case-insensitive.
func HasDraftTitleMarker(title string) bool {
	lower := strings.ToLower(strings.TrimSpace(title))
	for _, marker := range draftTitleMarkers {
		if strings.HasPrefix(lower, marker) {
			return true
		}
	}
	return false
}

// StripDraftTitleMarker removes a leading draft marker from title and trims
// the remainder; a title without a marker is returned trimmed only.
func StripDraftTitleMarker(title string) string {
	trimmed := strings.TrimSpace(title)
	lower := strings.ToLower(trimmed)
	for _, marker := range draftTitleMarkers {
		if strings.HasPrefix(lower, marker) {
			return strings.TrimSpace(trimmed[len(marker):])
		}
	}
	return trimmed
}
