package compactor

import (
	"context"
	"encoding/json"
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
	p := NewContextProvider()
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

func TestContextProvider_Rollback(t *testing.T) {
	p := NewContextProvider()
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

func TestContextProvider_NotFoundAndSessionGone(t *testing.T) {
	p := NewContextProvider()
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
