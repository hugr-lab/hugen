package perm

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/hugr-lab/hugen/pkg/auth/template"
)

func mergeFor(t *testing.T, conf, rem []Rule, object, field string) Permission {
	t.Helper()
	got, err := Merge(conf, rem, template.Context{}, object, field)
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	return got
}

func TestMerge_DisabledOR(t *testing.T) {
	cases := []struct {
		name string
		conf []Rule
		rem  []Rule
		want bool
	}{
		{"config_only", []Rule{{Type: "T", Field: "f", Disabled: true}}, nil, true},
		{"remote_only", nil, []Rule{{Type: "T", Field: "f", Disabled: true}}, true},
		{"both", []Rule{{Type: "T", Field: "f", Disabled: true}}, []Rule{{Type: "T", Field: "f", Disabled: true}}, true},
		{"neither", []Rule{{Type: "T", Field: "f"}}, []Rule{{Type: "T", Field: "f"}}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := mergeFor(t, tc.conf, tc.rem, "T", "f")
			if got.Disabled != tc.want {
				t.Errorf("Disabled = %v, want %v", got.Disabled, tc.want)
			}
		})
	}
}

func TestMerge_HiddenOR(t *testing.T) {
	got := mergeFor(t,
		[]Rule{{Type: "T", Field: "f", Hidden: true}},
		[]Rule{{Type: "T", Field: "f"}},
		"T", "f")
	if !got.Hidden {
		t.Errorf("Hidden = false; want true (config-only set)")
	}
}

func TestMerge_FilterAND(t *testing.T) {
	got := mergeFor(t,
		[]Rule{{Type: "T", Field: "f", Filter: "tenant_id = 7"}},
		[]Rule{{Type: "T", Field: "f", Filter: "deleted = false"}},
		"T", "f")
	if !strings.Contains(got.Filter, "tenant_id = 7") || !strings.Contains(got.Filter, "deleted = false") || !strings.Contains(got.Filter, "AND") {
		t.Errorf("Filter = %q, want both clauses AND-merged", got.Filter)
	}
}

func TestMerge_FilterOnlyOneSide(t *testing.T) {
	got := mergeFor(t,
		[]Rule{{Type: "T", Field: "f", Filter: "tenant_id = 7"}},
		[]Rule{{Type: "T", Field: "f"}},
		"T", "f")
	if got.Filter != "tenant_id = 7" {
		t.Errorf("Filter = %q, want pass-through of single side", got.Filter)
	}
}

func TestMerge_DataConfigWins(t *testing.T) {
	got := mergeFor(t,
		[]Rule{{Type: "T", Field: "f", Data: json.RawMessage(`{"limit":10,"region":"eu"}`)}},
		[]Rule{{Type: "T", Field: "f", Data: json.RawMessage(`{"limit":1000,"role":"analyst"}`)}},
		"T", "f")
	var dst struct {
		Limit  int    `json:"limit"`
		Region string `json:"region"`
		Role   string `json:"role"`
	}
	if err := json.Unmarshal(got.Data, &dst); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if dst.Limit != 10 {
		t.Errorf("limit = %d, want 10 (config wins)", dst.Limit)
	}
	if dst.Region != "eu" {
		t.Errorf("region = %q, want eu (config-only)", dst.Region)
	}
	if dst.Role != "analyst" {
		t.Errorf("role = %q, want analyst (remote-only)", dst.Role)
	}
}

func TestMerge_FromConfigFromRemoteFlags(t *testing.T) {
	cases := []struct {
		name             string
		conf, rem        []Rule
		wantConf, wantRem bool
	}{
		{"config_only", []Rule{{Type: "T", Field: "f", Disabled: true}}, nil, true, false},
		{"remote_only", nil, []Rule{{Type: "T", Field: "f", Disabled: true}}, false, true},
		{"both", []Rule{{Type: "T", Field: "f", Disabled: true}}, []Rule{{Type: "T", Field: "f"}}, true, true},
		{"neither_matched", []Rule{{Type: "X"}}, []Rule{{Type: "Y"}}, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := mergeFor(t, tc.conf, tc.rem, "T", "f")
			if got.FromConfig != tc.wantConf {
				t.Errorf("FromConfig = %v, want %v", got.FromConfig, tc.wantConf)
			}
			if got.FromRemote != tc.wantRem {
				t.Errorf("FromRemote = %v, want %v", got.FromRemote, tc.wantRem)
			}
		})
	}
}

func TestMerge_TemplateSubstitutionInData(t *testing.T) {
	tctx := template.Context{UserID: "u123", Role: "analyst"}
	conf := []Rule{{Type: "T", Field: "f", Data: json.RawMessage(`{"user":"[$auth.user_id]"}`)}}
	rem := []Rule{{Type: "T", Field: "f", Data: json.RawMessage(`{"role":"[$auth.role]"}`)}}
	got, err := Merge(conf, rem, tctx, "T", "f")
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	var m map[string]string
	if err := json.Unmarshal(got.Data, &m); err != nil {
		t.Fatal(err)
	}
	if m["user"] != "u123" {
		t.Errorf("user = %q", m["user"])
	}
	if m["role"] != "analyst" {
		t.Errorf("role = %q", m["role"])
	}
}

func TestMerge_WildcardThenExact(t *testing.T) {
	conf := []Rule{
		{Type: "T", Field: "*", Filter: "deleted = false"},
		{Type: "T", Field: "f", Filter: "id > 0"},
	}
	got := mergeFor(t, conf, nil, "T", "f")
	if !strings.Contains(got.Filter, "deleted = false") {
		t.Errorf("missing wildcard filter: %q", got.Filter)
	}
	if !strings.Contains(got.Filter, "id > 0") {
		t.Errorf("missing exact filter: %q", got.Filter)
	}
}

func TestMerge_EmptyInputsReturnZero(t *testing.T) {
	got := mergeFor(t, nil, nil, "T", "f")
	if got.Disabled || got.Hidden || got.Filter != "" || len(got.Data) != 0 {
		t.Errorf("non-zero permission for empty inputs: %+v", got)
	}
	if got.FromConfig || got.FromRemote {
		t.Errorf("From* flags set for empty inputs")
	}
}
