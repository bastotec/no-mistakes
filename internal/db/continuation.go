package db

import (
	"encoding/json"
	"fmt"
	"regexp"
)

// ContinuationPins is immutable run authority, not inferred from mutable refs.
// BaseTip may already be ahead of MergeBase: continuation never integrates it.
type ContinuationPins struct {
	Version    int    `json:"version"`
	PRHead     string `json:"pr_head"`
	BaseTip    string `json:"base_tip"`
	MergeBase  string `json:"merge_base"`
	BaseBranch string `json:"base_branch"`
	URL        string `json:"url"`
}

func (r *Run) ContinuationPins() (*ContinuationPins, error) {
	if r == nil || r.ContinuationJSON == nil {
		return nil, nil
	}
	var p ContinuationPins
	if err := json.Unmarshal([]byte(*r.ContinuationJSON), &p); err != nil || p.Version != 1 {
		return nil, fmt.Errorf("continuation: invalid durable policy")
	}
	sha := regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)
	if !sha.MatchString(p.PRHead) || !sha.MatchString(p.BaseTip) || !sha.MatchString(p.MergeBase) || p.URL == "" || p.BaseBranch == "" {
		return nil, fmt.Errorf("continuation: incomplete durable policy")
	}
	return &p, nil
}
