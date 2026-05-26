package scheduler

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/hugr-lab/hugen/pkg/extension"
	schedstore "github.com/hugr-lab/hugen/pkg/scheduler/store"
)

// callTool is a thin wrapper that injects the calling state into ctx
// and invokes the extension as the tool dispatcher would.
func callTool(t *testing.T, ext *Extension, state extension.SessionState, name string, args any) json.RawMessage {
	t.Helper()
	body, err := json.Marshal(args)
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}
	ctx := extension.WithSessionState(context.Background(), state)
	out, err := ext.Call(ctx, "task:"+name, body)
	if err != nil {
		t.Fatalf("Call task:%s: %v", name, err)
	}
	return out
}

func decodeAck(t *testing.T, body json.RawMessage) taskAckOutput {
	t.Helper()
	var ack taskAckOutput
	if err := json.Unmarshal(body, &ack); err != nil {
		t.Fatalf("decode ack: %v\nraw: %s", err, body)
	}
	return ack
}

func decodeList(t *testing.T, body json.RawMessage) listOutput {
	t.Helper()
	var out listOutput
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode list: %v\nraw: %s", err, body)
	}
	return out
}

func decodeErr(t *testing.T, body json.RawMessage) toolError {
	t.Helper()
	var resp toolErrorResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode err: %v\nraw: %s", err, body)
	}
	return resp.Error
}

func TestList_ReturnsOwnerScopedRows(t *testing.T) {
	ext, store, _, _ := withWiredExtension(t)
	owner := newFakeState("ses-owner-list")
	other := newFakeState("ses-foreign-list")

	seedTask(t, store, "ses-owner-list", "tsk_l1", schedstore.StatusActive, schedstore.KindWake)
	seedTask(t, store, "ses-owner-list", "tsk_l2", schedstore.StatusActive, schedstore.KindSpawn)
	seedTask(t, store, "ses-foreign-list", "tsk_other", schedstore.StatusActive, schedstore.KindWake)

	body := callTool(t, ext, owner, "list", map[string]any{})
	out := decodeList(t, body)
	if len(out.Tasks) != 2 {
		t.Fatalf("expected 2 owned tasks, got %d (%v)", len(out.Tasks), out.Tasks)
	}
	ids := []string{out.Tasks[0].TaskID, out.Tasks[1].TaskID}
	if !containsExact(ids, "tsk_l1") || !containsExact(ids, "tsk_l2") {
		t.Errorf("expected tsk_l1 + tsk_l2, got %v", ids)
	}
	for _, entry := range out.Tasks {
		if entry.NextPlannedAt == "" {
			t.Errorf("entry %q missing next_planned_at", entry.TaskID)
		}
	}

	// Foreign session sees only its own task.
	body2 := callTool(t, ext, other, "list", map[string]any{})
	out2 := decodeList(t, body2)
	if len(out2.Tasks) != 1 || out2.Tasks[0].TaskID != "tsk_other" {
		t.Errorf("foreign session list mismatch: %v", out2.Tasks)
	}
}

func TestList_StatusFilter(t *testing.T) {
	ext, store, _, _ := withWiredExtension(t)
	owner := newFakeState("ses-owner-statusfilter")
	seedTask(t, store, "ses-owner-statusfilter", "tsk_a", schedstore.StatusActive, schedstore.KindWake)
	seedTask(t, store, "ses-owner-statusfilter", "tsk_p", schedstore.StatusPaused, schedstore.KindWake)

	body := callTool(t, ext, owner, "list", map[string]any{"status": "paused"})
	out := decodeList(t, body)
	if len(out.Tasks) != 1 || out.Tasks[0].TaskID != "tsk_p" {
		t.Errorf("status filter mismatch: %v", out.Tasks)
	}
}

func TestPause_RejectsForeignOwner(t *testing.T) {
	ext, store, _, _ := withWiredExtension(t)
	stranger := newFakeState("ses-stranger")
	seedTask(t, store, "ses-real-owner", "tsk_target", schedstore.StatusActive, schedstore.KindSpawn)
	// First initiate from the real owner so InitState registers the
	// task with the runner — this proves Pause's runner.Pause path
	// is wired even for an owner-scoped guard rejection path.
	if err := ext.InitState(context.Background(), newFakeState("ses-real-owner")); err != nil {
		t.Fatalf("InitState (real owner): %v", err)
	}

	body := callTool(t, ext, stranger, "pause", map[string]any{"task_id": "tsk_target"})
	errBody := decodeErr(t, body)
	if errBody.Code != "forbidden" {
		t.Errorf("expected forbidden, got %v (%s)", errBody.Code, errBody.Message)
	}
	row, err := store.GetTask(context.Background(), "tsk_target")
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if row.Status != schedstore.StatusActive {
		t.Errorf("foreign pause should not mutate task; got status %q", row.Status)
	}
}

func TestPause_ResumeRoundTrip(t *testing.T) {
	ext, store, _, r := withWiredExtension(t)
	owner := newFakeState("ses-owner-pr")
	seedTask(t, store, "ses-owner-pr", "tsk_pr", schedstore.StatusActive, schedstore.KindSpawn)
	if err := ext.InitState(context.Background(), owner); err != nil {
		t.Fatalf("InitState: %v", err)
	}

	// Pause.
	body := callTool(t, ext, owner, "pause", map[string]any{"task_id": "tsk_pr"})
	ack := decodeAck(t, body)
	if ack.Status != schedstore.StatusPaused {
		t.Errorf("pause ack status=%q, want paused", ack.Status)
	}
	row, _ := store.GetTask(context.Background(), "tsk_pr")
	if row.Status != schedstore.StatusPaused {
		t.Errorf("row status after pause=%q", row.Status)
	}
	if row.PauseReason != schedstore.PauseUser {
		t.Errorf("default pause_reason = %q, want user", row.PauseReason)
	}
	st, _ := r.Status(context.Background(), "task_tsk_pr")
	if !st.Paused {
		t.Errorf("runner should be paused after pause tool")
	}

	// Resume.
	body = callTool(t, ext, owner, "resume", map[string]any{"task_id": "tsk_pr"})
	ack = decodeAck(t, body)
	if ack.Status != schedstore.StatusActive {
		t.Errorf("resume ack status=%q, want active", ack.Status)
	}
	row, _ = store.GetTask(context.Background(), "tsk_pr")
	if row.Status != schedstore.StatusActive {
		t.Errorf("row status after resume=%q", row.Status)
	}
	st, _ = r.Status(context.Background(), "task_tsk_pr")
	if st.Paused {
		t.Errorf("runner should be active after resume tool")
	}
}

func TestPause_CustomReason(t *testing.T) {
	ext, store, _, _ := withWiredExtension(t)
	owner := newFakeState("ses-owner-custom")
	seedTask(t, store, "ses-owner-custom", "tsk_c", schedstore.StatusActive, schedstore.KindWake)
	if err := ext.InitState(context.Background(), owner); err != nil {
		t.Fatalf("InitState: %v", err)
	}

	body := callTool(t, ext, owner, "pause",
		map[string]any{"task_id": "tsk_c", "reason": "schema_changed"})
	ack := decodeAck(t, body)
	if ack.Status != schedstore.StatusPaused {
		t.Errorf("status mismatch: %q", ack.Status)
	}
	row, _ := store.GetTask(context.Background(), "tsk_c")
	if row.PauseReason != "schema_changed" {
		t.Errorf("custom reason not propagated, got %q", row.PauseReason)
	}
}

func TestCancel_RemovesRunnerRegistration(t *testing.T) {
	ext, store, _, r := withWiredExtension(t)
	owner := newFakeState("ses-owner-cancel")
	seedTask(t, store, "ses-owner-cancel", "tsk_cx", schedstore.StatusActive, schedstore.KindSpawn)
	if err := ext.InitState(context.Background(), owner); err != nil {
		t.Fatalf("InitState: %v", err)
	}

	body := callTool(t, ext, owner, "cancel", map[string]any{"task_id": "tsk_cx"})
	ack := decodeAck(t, body)
	if ack.Status != schedstore.StatusCancelled {
		t.Errorf("cancel ack status=%q", ack.Status)
	}
	row, _ := store.GetTask(context.Background(), "tsk_cx")
	if row.Status != schedstore.StatusCancelled {
		t.Errorf("row status after cancel=%q", row.Status)
	}
	if _, ok := r.Status(context.Background(), "task_tsk_cx"); ok {
		t.Errorf("cancel must unregister the runner entry")
	}
}

func TestCancel_IdempotentOnUnknownTask(t *testing.T) {
	ext, _, _, _ := withWiredExtension(t)
	owner := newFakeState("ses-owner-unknown")

	body := callTool(t, ext, owner, "cancel", map[string]any{"task_id": "tsk_nope"})
	errBody := decodeErr(t, body)
	if errBody.Code != "not_found" {
		t.Errorf("expected not_found, got %v (%s)", errBody.Code, errBody.Message)
	}
}

func TestResume_RejectsCancelled(t *testing.T) {
	ext, store, _, _ := withWiredExtension(t)
	owner := newFakeState("ses-owner-cancres")
	seedTask(t, store, "ses-owner-cancres", "tsk_cr", schedstore.StatusCancelled, schedstore.KindWake)

	body := callTool(t, ext, owner, "resume", map[string]any{"task_id": "tsk_cr"})
	errBody := decodeErr(t, body)
	if errBody.Code != "invalid_state" {
		t.Errorf("expected invalid_state, got %v (%s)", errBody.Code, errBody.Message)
	}
}

func TestPause_MissingTaskID(t *testing.T) {
	ext, _, _, _ := withWiredExtension(t)
	owner := newFakeState("ses-noid")
	body := callTool(t, ext, owner, "pause", map[string]any{})
	errBody := decodeErr(t, body)
	if errBody.Code != "invalid_args" {
		t.Errorf("expected invalid_args, got %v (%s)", errBody.Code, errBody.Message)
	}
	if !strings.Contains(errBody.Message, "task_id") {
		t.Errorf("error message should mention task_id; got %q", errBody.Message)
	}
}

func containsExact(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
