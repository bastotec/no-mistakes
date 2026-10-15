package types

import "fmt"

// SurvivedWorkState says where a run's interrupted repair work lives now.
type SurvivedWorkState string

const (
	// SurvivedWorkCaptured: the work sits in Ref, not in the worktree.
	SurvivedWorkCaptured SurvivedWorkState = "captured"
	// SurvivedWorkRestored: a fix round put the work back into the worktree;
	// Ref still holds the capture it came from.
	SurvivedWorkRestored SurvivedWorkState = "restored"
	// SurvivedWorkInPlace: the work could not be captured and was left in
	// the worktree.
	SurvivedWorkInPlace SurvivedWorkState = "in_place"
)

// SurvivedWorkMaxFiles bounds the status lines kept as evidence, so a repair
// that touched a huge tree cannot grow a run snapshot past the IPC frame.
const SurvivedWorkMaxFiles = 50

// SurvivedWork is local-only evidence of an interrupted repair. It is stored
// per run, never in step findings, so an agent cannot forge it, and it is
// never included in PR summaries or attestations: Worktree names an operator
// path.
type SurvivedWork struct {
	State    SurvivedWorkState `json:"state"`
	Worktree string            `json:"worktree"`
	Files    []string          `json:"files,omitempty"`
	// Ref and BaseSHA locate the captured work: a run-owned local ref whose
	// commits sit on BaseSHA. Empty for in-place work.
	Ref             string `json:"ref,omitempty"`
	BaseSHA         string `json:"base_sha,omitempty"`
	InspectionError string `json:"inspection_error,omitempty"`
	Next            string `json:"next"`
}

// BoundSurvivedWorkFiles keeps the first SurvivedWorkMaxFiles lines and
// summarizes the rest.
func BoundSurvivedWorkFiles(files []string) []string {
	if len(files) <= SurvivedWorkMaxFiles {
		return files
	}
	bounded := append([]string(nil), files[:SurvivedWorkMaxFiles]...)
	return append(bounded, fmt.Sprintf("... and %d more", len(files)-SurvivedWorkMaxFiles))
}
