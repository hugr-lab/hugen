package mission

import (
	"errors"
	"fmt"
	"strings"
)

// AcceptanceCriterion is one identity-bearing row in a mission's
// acceptance criteria list. Carries the contract part (statement +
// origin + drop) and the status tracking (current + history of when
// it flipped + last evidence).
//
// IDs are runtime-assigned ("ac-1", "ac-2", …) — never picked by the
// planner — so re-emit / re-render across iterations addresses the
// same row even if the planner reworded the statement. Once an id is
// minted it never gets reused inside a mission, even if the row is
// dropped.
type AcceptanceCriterion struct {
	// ID is the stable identifier ("ac-N"). Runtime assigns at AC
	// creation time; the planner/checker/worker MUST reference rows
	// by this id, never by free-form statement text.
	ID string

	// Statement is the human-readable criterion text. Mutable across
	// iterations via planner ac_update, but every contract change
	// triggers re-approval (§3.2.1).
	Statement string

	// Origin tags where this criterion was first introduced — the
	// approval modal surfaces it so the user understands provenance.
	// One of the OriginXxx constants below; runtime stamps at create
	// time from the diff that introduced the row.
	Origin string

	// Status is the current satisfaction state. Three values:
	// unsatisfied (default) / satisfied / dropped.
	Status ACStatus

	// LastEvidence is free-form prose stamped on every status
	// transition explaining where the change came from. Examples:
	// "worker analyst handoff iter-2 wave-fetch", "checker iter-3",
	// "user refine in approval modal".
	LastEvidence string

	// AddedAtIter is the planner iteration this row was introduced
	// (1-indexed). 0 reserved for manifest- / user-seeded entries
	// (mission spawn, before planner runs).
	AddedAtIter int

	// SatisfiedAtIter is the iteration the row first reached
	// status=satisfied. 0 when never satisfied.
	SatisfiedAtIter int

	// DroppedAtIter is the iteration the row was dropped (status set
	// to ACDropped). 0 when not dropped.
	DroppedAtIter int

	// DropReason is the planner / user-supplied reason set alongside
	// a drop diff. Empty unless Status=ACDropped.
	DropReason string
}

// ACStatus is the three-state status enum.
type ACStatus string

const (
	// ACUnsatisfied is the default for a freshly-created row.
	ACUnsatisfied ACStatus = "unsatisfied"
	// ACSatisfied marks rows the checker / worker confirmed met.
	ACSatisfied ACStatus = "satisfied"
	// ACDropped marks rows the planner removed via ac_update.drop or
	// the user refined out — excluded from finish-gate but kept in
	// history for audit.
	ACDropped ACStatus = "dropped"
)

// Origin tags. Values cover every channel B11 §3.2.2 enumerates.
const (
	// OriginManifest — seeded from skill manifest's
	// mission.acceptance_criteria at mission spawn (iter 0).
	OriginManifest = "manifest"
	// OriginUserInitial — extracted from the user's goal text at
	// mission spawn (iter 0). Currently the runtime does not extract
	// criteria from prose; reserved for §3.2.2 future channel.
	OriginUserInitial = "user_initial"
	// OriginResearchProposal — accepted from research.ac_proposals
	// the planner promoted via ac_add on iter 1.
	OriginResearchProposal = "research_proposal"
	// OriginPlanner — added/edited by the planner on iter N.
	// Suffix is the iteration number ("planner_iter_2").
	OriginPlannerPrefix = "planner_iter_"
	// OriginUserRefine — added/edited by the user via the approval
	// modal's refine path.
	OriginUserRefine = "user_refine"
)

// PlannerOriginAt builds the canonical "planner_iter_N" origin tag.
func PlannerOriginAt(iter int) string {
	if iter < 1 {
		return OriginPlannerPrefix + "0"
	}
	return fmt.Sprintf("%s%d", OriginPlannerPrefix, iter)
}

// ACAddSpec is one ac_add entry emitted by a planner. Runtime mints
// an ID and stamps origin from PlannerOriginAt(iter). The user-refine
// path also emits this shape, overriding origin to OriginUserRefine.
//
// Statement is required; an empty statement is rejected at parse
// time. Origin is optional on the wire — runtime fills it from the
// context (planner_iter_N, research_proposal, user_refine).
type ACAddSpec struct {
	Statement string `json:"statement" yaml:"statement"`
	// Origin lets callers (e.g. research_proposal → planner) tag the
	// row with a specific provenance. Optional; runtime fills with
	// PlannerOriginAt when empty.
	Origin string `json:"origin,omitempty" yaml:"origin,omitempty"`
}

// ACUpdateSpec is one ac_update entry. Channels:
//
//   - Planner — may set Statement (rewrites contract), Drop (with
//     DropReason), Status (with optional Evidence). Contract fields
//     (Statement / Drop) trigger re-approval; status-only does not.
//
//   - Checker — only Status + Evidence are honoured. Entries carrying
//     Statement / Drop are rejected by the validator (planner is the
//     contract authority — checker raises issues if it disagrees).
//
//   - User-refine — same shape as planner (full authority inside the
//     approval modal).
type ACUpdateSpec struct {
	// ID is the target row id ("ac-N"). Required.
	ID string `json:"id" yaml:"id"`

	// Statement, when non-empty, rewrites the row's statement.
	// Planner / user-refine only. Triggers re-approval.
	Statement string `json:"statement,omitempty" yaml:"statement,omitempty"`

	// Drop, when true, marks the row dropped (excluded from
	// finish-gate). Planner / user-refine only. Triggers re-approval.
	Drop bool `json:"drop,omitempty" yaml:"drop,omitempty"`

	// DropReason is the planner's / user's justification for the
	// drop. Required when Drop=true; ignored otherwise.
	DropReason string `json:"drop_reason,omitempty" yaml:"drop_reason,omitempty"`

	// Status, when non-empty, updates the row's status. Permissible
	// values: ACUnsatisfied / ACSatisfied. ACDropped is reachable
	// only via Drop=true (avoids two-channel ambiguity).
	Status ACStatus `json:"status,omitempty" yaml:"status,omitempty"`

	// Evidence is free-form prose stamped on the row's LastEvidence
	// when this update applies. Used by checker / planner status
	// updates; worker satisfies channel synthesises its own evidence
	// string.
	Evidence string `json:"evidence,omitempty" yaml:"evidence,omitempty"`
}

// IsContractChange reports whether this update carries a field that
// rewrites the contract (Statement or Drop). Auto-promotes
// requires_reapproval per §3.2.1.
func (u ACUpdateSpec) IsContractChange() bool {
	return u.Statement != "" || u.Drop
}

// ACDiff bundles the per-iter changes from one emitter (planner,
// checker, user-refine). Runtime applies via [MissionState.ApplyACDiff]
// (planner / user-refine) or [MissionState.ApplyStatusOnly] (checker).
type ACDiff struct {
	Add    []ACAddSpec    `json:"ac_add,omitempty" yaml:"ac_add,omitempty"`
	Update []ACUpdateSpec `json:"ac_update,omitempty" yaml:"ac_update,omitempty"`
}

// IsEmpty reports whether the diff carries no rows.
func (d ACDiff) IsEmpty() bool {
	return len(d.Add) == 0 && len(d.Update) == 0
}

// HasContractChange reports whether any entry in the diff is a
// contract change (ac_add OR ac_update with Statement/Drop). Used by
// runtime to auto-promote requires_reapproval per §3.2.1 even when
// the planner forgot to set the flag.
func (d ACDiff) HasContractChange() bool {
	if len(d.Add) > 0 {
		return true
	}
	for _, u := range d.Update {
		if u.IsContractChange() {
			return true
		}
	}
	return false
}

// ValidatePlannerDiff returns nil iff every Add / Update entry is
// shape-valid for the planner / user-refine channel. Rules per
// §3.2.1 + §3.2.2:
//
//   - ac_add[i].statement must be non-empty.
//   - ac_update[i].id must be non-empty.
//   - ac_update[i] must carry at least one update field (Statement,
//     Drop, Status — Evidence alone is not a change).
//   - ac_update[i].drop=true requires DropReason non-empty.
//   - ac_update[i].status, if set, must be ACUnsatisfied or
//     ACSatisfied (ACDropped is reachable only via Drop=true).
//
// Caller is responsible for id-existence checks (those need state
// context, not just the diff shape).
func ValidatePlannerDiff(d ACDiff) error {
	for i, a := range d.Add {
		if strings.TrimSpace(a.Statement) == "" {
			return fmt.Errorf("ac_add[%d]: statement must be non-empty", i)
		}
	}
	for i, u := range d.Update {
		if strings.TrimSpace(u.ID) == "" {
			return fmt.Errorf("ac_update[%d]: id must be non-empty", i)
		}
		if u.Statement == "" && !u.Drop && u.Status == "" {
			return fmt.Errorf("ac_update[%d]: at least one of statement / drop / status must be set", i)
		}
		if u.Drop && strings.TrimSpace(u.DropReason) == "" {
			return fmt.Errorf("ac_update[%d]: drop=true requires drop_reason", i)
		}
		switch u.Status {
		case "", ACUnsatisfied, ACSatisfied:
			// ok
		case ACDropped:
			return fmt.Errorf("ac_update[%d]: status=dropped is not a valid wire value — set drop=true with drop_reason instead", i)
		default:
			return fmt.Errorf("ac_update[%d]: status=%q is not one of unsatisfied/satisfied", i, u.Status)
		}
	}
	return nil
}

// ValidateCheckerDiff is the stricter validator for checker output.
// Same as ValidatePlannerDiff but rejects every entry carrying
// Statement or Drop — those belong to the planner's authority.
//
// Checker does NOT emit ac_add (contract changes are planner-only).
func ValidateCheckerDiff(d ACDiff) error {
	if len(d.Add) > 0 {
		return errors.New("ac_add[]: checker cannot add new acceptance criteria — emit an issue in the verdict body and let the planner decide")
	}
	for i, u := range d.Update {
		if strings.TrimSpace(u.ID) == "" {
			return fmt.Errorf("ac_update[%d]: id must be non-empty", i)
		}
		if u.Statement != "" || u.Drop {
			return fmt.Errorf("ac_update[%d]: checker cannot rewrite statement / drop a criterion — raise an issue in the verdict body instead", i)
		}
		if u.Status == "" {
			return fmt.Errorf("ac_update[%d]: status must be set (unsatisfied|satisfied) for checker updates", i)
		}
		switch u.Status {
		case ACUnsatisfied, ACSatisfied:
			// ok
		default:
			return fmt.Errorf("ac_update[%d]: status=%q is not one of unsatisfied/satisfied", i, u.Status)
		}
	}
	return nil
}
