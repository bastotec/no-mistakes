package cli

import (
	"github.com/kunchenguid/no-mistakes/internal/db"
	toon "github.com/toon-format/toon-go"
)

// selectedRunCustodyField is observation only. Alignment and absent legacy
// stamps never prove returned edit ownership. Cached sync describes the caller,
// not an arbitrary selected historical branch, and is labelled accordingly.
func selectedRunCustodyField(d *db.DB, selected *db.Run, cached *cachedBranchSyncObservation) toon.Field {
	owner, err := d.GetActiveRun(selected.RepoID, selected.Branch)
	ownership, ownerID, ownerHead := "unknown", "", ""
	if err == nil && owner != nil {
		ownership, ownerID, ownerHead = "active_run", owner.ID, owner.HeadSHA
	}
	returned := "unknown_legacy_or_not_returned"
	if selected.CustodyReturnedAt != nil {
		returned = "recorded_for_selected_run_only"
	}
	callerMatches := cached != nil && cached.repoID == selected.RepoID && cached.state.Local.Branch == selected.Branch
	local, alignment := "", "unknown"
	if callerMatches && cached.state.Local.Head != "" {
		local = cached.state.Local.Head
		alignment = "different"
		if local == selected.HeadSHA {
			alignment = "equal_not_custody_proof"
		}
	}
	pinState := "unknown"
	if eligible, readErr := d.PublicationRecoveryEligible(selected.ID); readErr == nil {
		pinState = "legacy_not_eligible"
		if eligible {
			pinState = "missing_not_eligible"
			if pin, pinErr := d.GetPublicationIntent(selected.ID, selected.HeadSHA); pinErr != nil {
				pinState = "unknown"
			} else if pin != nil {
				pinState = "present_not_proof_of_eligibility"
			}
		}
	}
	fields := []toon.Field{
		{Key: "publication_pins", Value: pinState},
		{Key: "selected_run_id", Value: selected.ID},
		{Key: "selected_branch", Value: selected.Branch},
		{Key: "selected_head", Value: selected.HeadSHA},
		{Key: "ownership", Value: ownership},
		{Key: "active_owner_run_id", Value: ownerID},
		{Key: "active_owner_head", Value: ownerHead},
		{Key: "selected_custody_return", Value: returned},
		{Key: "local_head", Value: local},
		{Key: "head_alignment", Value: alignment},
		{Key: "remote_binding", Value: "unknown_not_freshly_observed"},
	}
	// The latest branch record may be terminal yet retain custody. Report it
	// separately; no active run is not proof that the caller owns the branch.
	if runs, readErr := d.GetRunsByRepo(selected.RepoID); readErr == nil {
		for _, current := range runs {
			if current.Branch != selected.Branch {
				continue
			}
			fields = append(fields,
				toon.Field{Key: "current_branch_run_id", Value: current.ID},
				toon.Field{Key: "current_branch_run_status", Value: string(current.Status)},
				toon.Field{Key: "current_branch_run_head", Value: current.HeadSHA})
			break
		}
	}
	if callerMatches {
		state := cached.state
		fields = append(fields,
			toon.Field{Key: "cached_branch_run_id", Value: state.Pipeline.RunID},
			toon.Field{Key: "cached_branch_state", Value: state.State},
			toon.Field{Key: "cached_remote_head", Value: state.Remote.ObservedHead},
			toon.Field{Key: "cached_remote_freshness", Value: state.Remote.Freshness})
	}
	if selected.LastPushedSHA != nil {
		fields = append(fields, toon.Field{Key: "selected_recorded_pushed_head", Value: *selected.LastPushedSHA})
	}
	if selected.PushTargetFingerprint != nil {
		fields = append(fields, toon.Field{Key: "selected_recorded_target", Value: *selected.PushTargetFingerprint})
	}
	if selected.CustodyReturnedAt != nil {
		fields = append(fields, toon.Field{Key: "selected_custody_returned_at", Value: *selected.CustodyReturnedAt})
	}
	if err != nil {
		fields = append(fields, toon.Field{Key: "owner_read", Value: "failed_unknown"})
	}
	return toon.Field{Key: "selected_run_custody", Value: toon.NewObject(fields...)}
}
