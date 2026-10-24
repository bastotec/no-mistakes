package ipc

// RefreshPublicationParams is an exact binding, never a latest-run selector.
type RefreshPublicationParams struct {
	RepoID  string `json:"repo_id"`
	RunID   string `json:"run_id"`
	HeadSHA string `json:"head_sha"`
	PRURL   string `json:"pr_url"`
}

type RefreshPublicationResult struct {
	RunID       string `json:"run_id"`
	HeadSHA     string `json:"head_sha"`
	PRURL       string `json:"pr_url"`
	Disposition string `json:"disposition"`
}
