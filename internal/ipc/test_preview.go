package ipc

// PublishTestPreviewParams binds operator authority to one repository, run and
// exact current head. No branch lookup or "latest run" fallback is permitted.
type PublishTestPreviewParams struct {
	RepoID  string `json:"repo_id"`
	RunID   string `json:"run_id"`
	HeadSHA string `json:"head_sha"`
}

type PublishTestPreviewResult struct {
	RunID      string `json:"run_id"`
	HeadSHA    string `json:"head_sha"`
	PRURL      string `json:"pr_url"`
	PreviewURL string `json:"preview_url,omitempty"`
	Label      string `json:"label"`
	TestStatus string `json:"test_status"`
}
