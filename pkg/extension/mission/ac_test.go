package mission

import (
	"strings"
	"testing"
)

func TestACUpdateSpec_IsContractChange(t *testing.T) {
	cases := []struct {
		name string
		u    ACUpdateSpec
		want bool
	}{
		{"status-only", ACUpdateSpec{ID: "ac-1", Status: ACSatisfied}, false},
		{"evidence-only is not a change at the wire-shape level", ACUpdateSpec{ID: "ac-1", Evidence: "x"}, false},
		{"statement is contract", ACUpdateSpec{ID: "ac-1", Statement: "new text"}, true},
		{"drop is contract", ACUpdateSpec{ID: "ac-1", Drop: true, DropReason: "scope"}, true},
		{"status+statement still contract", ACUpdateSpec{ID: "ac-1", Statement: "x", Status: ACSatisfied}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.u.IsContractChange(); got != tc.want {
				t.Errorf("IsContractChange()=%v, want %v", got, tc.want)
			}
		})
	}
}

func TestACDiff_HasContractChange(t *testing.T) {
	cases := []struct {
		name string
		d    ACDiff
		want bool
	}{
		{"empty", ACDiff{}, false},
		{"only status updates", ACDiff{Update: []ACUpdateSpec{{ID: "ac-1", Status: ACSatisfied}}}, false},
		{"has add", ACDiff{Add: []ACAddSpec{{Statement: "x"}}}, true},
		{"update with statement", ACDiff{Update: []ACUpdateSpec{{ID: "ac-1", Statement: "x"}}}, true},
		{"update with drop", ACDiff{Update: []ACUpdateSpec{{ID: "ac-1", Drop: true, DropReason: "x"}}}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.d.HasContractChange(); got != tc.want {
				t.Errorf("HasContractChange()=%v, want %v", got, tc.want)
			}
		})
	}
}

func TestValidatePlannerDiff(t *testing.T) {
	cases := []struct {
		name    string
		d       ACDiff
		wantErr string // substring; "" → no error expected
	}{
		{
			"empty is fine",
			ACDiff{},
			"",
		},
		{
			"ac_add empty statement rejected",
			ACDiff{Add: []ACAddSpec{{Statement: "  "}}},
			"statement must be non-empty",
		},
		{
			"ac_update empty id rejected",
			ACDiff{Update: []ACUpdateSpec{{Status: ACSatisfied}}},
			"id must be non-empty",
		},
		{
			"ac_update with no fields rejected",
			ACDiff{Update: []ACUpdateSpec{{ID: "ac-1"}}},
			"at least one of statement / drop / status",
		},
		{
			"ac_update drop without reason rejected",
			ACDiff{Update: []ACUpdateSpec{{ID: "ac-1", Drop: true}}},
			"drop=true requires drop_reason",
		},
		{
			"status=dropped via wire rejected (must use drop)",
			ACDiff{Update: []ACUpdateSpec{{ID: "ac-1", Status: ACDropped}}},
			"status=dropped is not a valid wire value",
		},
		{
			"unknown status rejected",
			ACDiff{Update: []ACUpdateSpec{{ID: "ac-1", Status: ACStatus("pending")}}},
			"not one of unsatisfied/satisfied",
		},
		{
			"valid planner diff",
			ACDiff{
				Add: []ACAddSpec{{Statement: "Include weekly comparison"}},
				Update: []ACUpdateSpec{
					{ID: "ac-2", Status: ACSatisfied, Evidence: "wave-1 handoff"},
					{ID: "ac-3", Drop: true, DropReason: "out of scope"},
				},
			},
			"",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidatePlannerDiff(tc.d)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error %q does not contain %q", err.Error(), tc.wantErr)
			}
		})
	}
}

func TestValidateCheckerDiff(t *testing.T) {
	cases := []struct {
		name    string
		d       ACDiff
		wantErr string
	}{
		{"empty is fine", ACDiff{}, ""},
		{
			"checker cannot ac_add",
			ACDiff{Add: []ACAddSpec{{Statement: "new"}}},
			"checker cannot add",
		},
		{
			"checker cannot rewrite statement",
			ACDiff{Update: []ACUpdateSpec{{ID: "ac-1", Statement: "rewrite"}}},
			"checker cannot rewrite statement",
		},
		{
			"checker cannot drop",
			ACDiff{Update: []ACUpdateSpec{{ID: "ac-1", Drop: true, DropReason: "scope"}}},
			"checker cannot rewrite statement / drop",
		},
		{
			"checker must set status",
			ACDiff{Update: []ACUpdateSpec{{ID: "ac-1", Evidence: "something"}}},
			"status must be set",
		},
		{
			"checker status-only is fine",
			ACDiff{Update: []ACUpdateSpec{
				{ID: "ac-1", Status: ACSatisfied, Evidence: "wave-1 produced file"},
				{ID: "ac-2", Status: ACUnsatisfied, Evidence: "missing comparison block"},
			}},
			"",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateCheckerDiff(tc.d)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error %q does not contain %q", err.Error(), tc.wantErr)
			}
		})
	}
}

func TestPlannerOriginAt(t *testing.T) {
	cases := []struct {
		iter int
		want string
	}{
		{1, "planner_iter_1"},
		{2, "planner_iter_2"},
		{0, "planner_iter_0"},
		{-1, "planner_iter_0"},
	}
	for _, tc := range cases {
		got := PlannerOriginAt(tc.iter)
		if got != tc.want {
			t.Errorf("PlannerOriginAt(%d)=%q, want %q", tc.iter, got, tc.want)
		}
	}
}
