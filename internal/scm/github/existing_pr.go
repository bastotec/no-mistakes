package github

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/scm"
)

// ExistingPRTarget accepts a canonical github.com PR URL. Cross-host account
// routing and other providers are deliberately not inferred from user input.
func ExistingPRTarget(raw string) (repo, number string, err error) {
	n, err := parsePullRequestURL(raw, "github.com", "")
	if err != nil {
		return "", "", err
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", "", err
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	repo = parts[0] + "/" + parts[1]
	number = strconv.Itoa(n)
	if raw != "https://github.com/"+repo+"/pull/"+number {
		return "", "", fmt.Errorf("--existing-pr requires a canonical https://github.com/owner/repo/pull/number URL")
	}
	return repo, number, nil
}

// ValidateExistingPR uses a numbered REST lookup, never branch discovery. Both
// repositories, the source ref and exact live head must be proven before use.
func (h *Host) ValidateExistingPR(ctx context.Context, raw, sourceRepo, branch, head string) (*scm.PR, error) {
	repo, number, err := ExistingPRTarget(raw)
	if err != nil {
		return nil, err
	}
	if h.host != "github.com" || !strings.EqualFold(repo, h.repoSlug()) {
		return nil, fmt.Errorf("explicit PR repository does not match selected host")
	}
	out, err := h.cmd(ctx, "gh", "api", "--hostname", "github.com", "repos/"+repo+"/pulls/"+number).Output()
	if err != nil {
		return nil, fmt.Errorf("validate explicit PR: %w", err)
	}
	var p struct {
		Number int    `json:"number"`
		URL    string `json:"html_url"`
		State  string `json:"state"`
		Merged bool   `json:"merged"`
		Base   struct {
			Ref  string `json:"ref"`
			Repo struct {
				FullName string `json:"full_name"`
				URL      string `json:"html_url"`
			} `json:"repo"`
		} `json:"base"`
		Head struct {
			Ref  string `json:"ref"`
			SHA  string `json:"sha"`
			Repo struct {
				FullName string `json:"full_name"`
				URL      string `json:"html_url"`
			} `json:"repo"`
		} `json:"head"`
	}
	if err := json.Unmarshal(out, &p); err != nil {
		return nil, fmt.Errorf("decode explicit PR: %w", err)
	}
	if strconv.Itoa(p.Number) != number || p.URL != raw || !strings.EqualFold(p.Base.Repo.FullName, repo) || !strings.EqualFold(p.Base.Repo.URL, "https://github.com/"+repo) {
		return nil, fmt.Errorf("explicit PR response repository/identity mismatch")
	}
	if p.State != "open" || p.Merged || p.Base.Ref == "" {
		return nil, fmt.Errorf("explicit PR is not an open, readable review object")
	}
	if sourceRepo == "" || !strings.EqualFold(p.Head.Repo.FullName, sourceRepo) || !strings.EqualFold(p.Head.Repo.URL, "https://github.com/"+sourceRepo) || p.Head.Ref != strings.TrimPrefix(branch, "refs/heads/") {
		return nil, fmt.Errorf("explicit PR source repository/ref does not match the configured push target and run branch")
	}
	if head == "" || p.Head.SHA != head {
		return nil, fmt.Errorf("explicit PR head does not match expected head %s", head)
	}
	return &scm.PR{Number: number, URL: raw, BaseBranch: p.Base.Ref}, nil
}
