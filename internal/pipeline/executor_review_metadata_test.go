package pipeline

import (
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// A rereview that reports nothing new merges back to the held findings; the
// result still carries the rereview's own level and unreviewed angles.
func TestWithReviewRoundMetadataKeepsTheFreshReviewsMetadata(t *testing.T) {
	held := `{"findings":[{"id":"held","severity":"warning","description":"deferred","action":"ask-user"}],"review_effort":"high"}`
	fresh := `{"findings":[],"review_effort":"xhigh","review_effort_reason":"large change","review_unreviewed":["security"]}`
	merged := withReviewRoundMetadata(mergeFindingsJSON(removeMatchingReviewHeldFindingsJSON(fresh, held), held), fresh)
	f, err := types.ParseFindingsJSON(merged)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Items) != 1 || f.Items[0].ID != "held" || f.ReviewEffort != "xhigh" || len(f.ReviewUnreviewed) != 1 || f.ReviewUnreviewed[0] != "security" {
		t.Fatalf("merged = %+v", f)
	}
}
