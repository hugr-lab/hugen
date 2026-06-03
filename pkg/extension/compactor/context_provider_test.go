package compactor

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/hugr-lab/hugen/pkg/extension"
	"github.com/hugr-lab/hugen/pkg/model"
)

func ctxWith(st *fakeState) context.Context {
	return extension.WithSessionState(context.Background(), st)
}

func callContext(t *testing.T, p *ContextProvider, ctx context.Context, name, args string) map[string]any {
	t.Helper()
	raw, err := p.Call(ctx, name, json.RawMessage(args))
	if err != nil {
		t.Fatalf("Call %s: %v", name, err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode %s result: %v", name, err)
	}
	return out
}

func TestContextProvider_ListAdvertisesFourTools(t *testing.T) {
	tools, err := (&ContextProvider{}).List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	want := map[string]bool{
		"context:checkpoint": false, "context:hide": false,
		"context:expand": false, "context:rollback": false,
	}
	for _, tl := range tools {
		if _, ok := want[tl.Name]; !ok {
			t.Fatalf("unexpected tool %q", tl.Name)
		}
		want[tl.Name] = true
		if tl.Provider != ContextProviderName {
			t.Fatalf("tool %q provider = %q, want %q", tl.Name, tl.Provider, ContextProviderName)
		}
	}
	for name, seen := range want {
		if !seen {
			t.Fatalf("missing tool %q", name)
		}
	}
}

func TestContextProvider_CheckpointHideExpand(t *testing.T) {
	p := NewContextProvider(nil)
	st, cs := stateWithCheckpoints("ses-prov")
	ctx := ctxWith(st)
	appendEntry(cs, 1, model.RoleAssistant, bigContent(50))
	appendEntry(cs, 2, model.RoleTool, bigContent(50))

	cpRes := callContext(t, p, ctx, "context:checkpoint", `{"description":"read data"}`)
	if cpRes["ok"] != true || cpRes["checkpoint"] != "cp-1" {
		t.Fatalf("checkpoint result = %+v", cpRes)
	}
	if cs.LastCheckpointSeq() != 2 {
		t.Fatalf("checkpoint did not advance lastCheckpointSeq; got %d", cs.LastCheckpointSeq())
	}

	hideRes := callContext(t, p, ctx, "context:hide", `{"cp_id":"cp-1"}`)
	if hideRes["ok"] != true || hideRes["hidden"] != "cp-1" {
		t.Fatalf("hide result = %+v", hideRes)
	}
	if cps := cs.Checkpoints(); len(cps) != 1 || !cps[0].Hidden {
		t.Fatalf("cp-1 not marked hidden: %+v", cps)
	}

	expRes := callContext(t, p, ctx, "context:expand", `{"cp_id":"cp-1"}`)
	if expRes["ok"] != true || expRes["expanded"] != "cp-1" {
		t.Fatalf("expand result = %+v", expRes)
	}
	if cps := cs.Checkpoints(); cps[0].Hidden {
		t.Fatalf("cp-1 still hidden after expand")
	}
}

// TestContextProvider_HideNoteFallback pins that with no summariser
// wired (nil), the agent's `note` is used verbatim as the placeholder
// brief (the fallback), taking precedence over the checkpoint label.
// The auto-summary path itself is covered by TestSummarizeSegment + the
// hide_summary unit; here we pin the note fallback + the rename.
func TestContextProvider_HideNoteFallback(t *testing.T) {
	p := NewContextProvider(nil)
	st, cs := stateWithCheckpoints("ses-note")
	ctx := ctxWith(st)
	appendEntry(cs, 1, model.RoleUser, "task")
	appendToolPair(cs, 2, "c1", "read_file", bigContent(50))
	cs.AddCheckpoint("raw discovery") // cp-1, label = "raw discovery"

	callContext(t, p, ctx, "context:hide",
		`{"cp_id":"cp-1","note":"op2023: 4 tables, key field Total_Amount_USD"}`)

	cp, _ := cs.FindCheckpoint("cp-1")
	if cp.Note != "op2023: 4 tables, key field Total_Amount_USD" {
		t.Fatalf("hide note not stored on checkpoint: %q", cp.Note)
	}

	// The placeholder must carry the note, not the checkpoint label.
	ext := newTestExtension(t)
	out := ext.ProvideHistory(context.Background(), st)
	gotNote := false
	for _, m := range out {
		if strings.Contains(m.Content, "op2023: 4 tables") {
			gotNote = true
		}
		if strings.Contains(m.Content, "raw discovery") {
			t.Fatalf("placeholder used the checkpoint label instead of the note: %q", m.Content)
		}
	}
	if !gotNote {
		t.Fatalf("placeholder did not carry the hide note; got %+v", out)
	}
}

// TestCheckpointResult_ShowsFill pins that context:* results surface the
// real context-fill so the model decides hide rationally.
func TestCheckpointResult_ShowsFill(t *testing.T) {
	p := NewContextProvider(nil)
	st, cs := stateWithCheckpoints("ses-fill")
	cs.SetOccupancy(36000, 100000, 80000) // as the controller would stamp
	ctx := ctxWith(st)
	appendEntry(cs, 1, model.RoleAssistant, "x")

	res := callContext(t, p, ctx, "context:checkpoint", `{"description":"d"}`)
	fill, _ := res["context_fill"].(string)
	if !strings.Contains(fill, "36%") {
		t.Fatalf("checkpoint result context_fill = %q, want a fill%%", res["context_fill"])
	}
	if u, _ := res["context_used_tokens"].(float64); u != 36000 {
		t.Fatalf("context_used_tokens = %v, want 36000", res["context_used_tokens"])
	}
}

func TestContextProvider_Rollback(t *testing.T) {
	p := NewContextProvider(nil)
	st, cs := stateWithCheckpoints("ses-rb")
	ctx := ctxWith(st)
	appendEntry(cs, 1, model.RoleUser, "task")
	appendEntry(cs, 2, model.RoleTool, "a")
	cs.AddCheckpoint("seg1") // cp-1 @ seq2
	appendEntry(cs, 3, model.RoleAssistant, "bad work")
	appendEntry(cs, 4, model.RoleAssistant, "rollback call") // in-flight call

	res := callContext(t, p, ctx, "context:rollback", `{"cp_id":"cp-1","note":"wrong path"}`)
	if res["ok"] != true {
		t.Fatalf("rollback result = %+v", res)
	}
	// JSON numbers decode to float64.
	if d, _ := res["entries_dropped"].(float64); d != 1 {
		t.Fatalf("entries_dropped = %v, want 1", res["entries_dropped"])
	}
}

// TestContextProvider_RollbackRequiresNote pins the side-effect contract:
// rollback without a note is rejected (the note is the model's only
// memory of physical work that rollback drops the context of but does
// NOT undo).
func TestContextProvider_RollbackRequiresNote(t *testing.T) {
	p := NewContextProvider(nil)
	st, cs := stateWithCheckpoints("ses-rbn")
	ctx := ctxWith(st)
	appendEntry(cs, 1, model.RoleUser, "task")
	appendEntry(cs, 2, model.RoleTool, "a")
	cs.AddCheckpoint("seg1")
	appendEntry(cs, 3, model.RoleAssistant, "work")

	res := callContext(t, p, ctx, "context:rollback", `{"cp_id":"cp-1"}`)
	errObj, _ := res["error"].(map[string]any)
	if errObj == nil || errObj["code"] != "bad_request" {
		t.Fatalf("rollback without note = %+v, want error.code=bad_request", res)
	}
	// And nothing was dropped (rejected before mutation).
	if cs.LastCheckpointSeq() != 2 || len(cs.historySnapshot()) != 3 {
		t.Fatalf("rejected rollback must not mutate state")
	}
}

func TestContextProvider_NotFoundAndSessionGone(t *testing.T) {
	p := NewContextProvider(nil)
	st, _ := stateWithCheckpoints("ses-err")

	// Unknown checkpoint → structured not_found.
	res := callContext(t, p, ctxWith(st), "context:hide", `{"cp_id":"cp-404"}`)
	errObj, _ := res["error"].(map[string]any)
	if errObj == nil || errObj["code"] != "not_found" {
		t.Fatalf("hide of unknown cp = %+v, want error.code=not_found", res)
	}

	// No session on ctx → session_gone.
	res2 := callContext(t, p, context.Background(), "context:checkpoint", `{"description":"x"}`)
	errObj2, _ := res2["error"].(map[string]any)
	if errObj2 == nil || errObj2["code"] != "session_gone" {
		t.Fatalf("no-session checkpoint = %+v, want error.code=session_gone", res2)
	}
}
