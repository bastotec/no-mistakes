package ipc

// RepublishPublicationParams binds operator authority to one repository, run
// and the exact recorded head the terminal run validated. No branch lookup or
// "latest run" fallback is permitted: the caller names the run explicitly and
// proves which head it expects, exactly like PublishTestPreviewParams.
type RepublishPublicationParams struct {
	RepoID  string `json:"repo_id"`
	RunID   string `json:"run_id"`
	HeadSHA string `json:"head_sha"`
}

// RepublishPublicationResult reports the publication URL and recorded step
// statuses as name:status strings, excluding PR and CI. Steps is a status
// summary, not the attestation's full step objects. Nothing here re-runs a step.
type RepublishPublicationResult struct {
	RunID   string   `json:"run_id"`
	HeadSHA string   `json:"head_sha"`
	PRURL   string   `json:"pr_url"`
	Steps   []string `json:"steps,omitempty"`
}
