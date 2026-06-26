package artifact

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"sync/atomic"

	"github.com/hugr-lab/hugen/pkg/extension"
	wsext "github.com/hugr-lab/hugen/pkg/extension/workspace"
	"github.com/hugr-lab/hugen/pkg/protocol"
	"github.com/hugr-lab/hugen/pkg/tool"
)

// StateKey is the [extension.SessionState] key the per-session
// [sessionArtifacts] handle is stored under.
const StateKey = "artifact"

const providerName = "artifact"

// Permission objects gated by the 3-tier permission stack. Per-tier
// narrowing (root gets all four; mission/worker get list+copy) is the
// tier SKILL.md allowed-tools' job — the extension exposes the full
// surface.
const (
	PermList    = "hugen:artifact:list"
	PermCopy    = "hugen:artifact:copy"
	PermPublish = "hugen:artifact:publish"
	PermDelete  = "hugen:artifact:delete"
)

// Operations carried on the artifact ExtensionFrame (CategoryMarker —
// adapter / audit record, never injected into the model prompt nor
// replayed by Recovery). The data payload is a [protocol.ArtifactRef].
const (
	OpUploaded = "artifact_uploaded" // a user file entered the store
	OpProduced = "artifact_produced" // a session published a file
)

// Extension is the agent-level singleton wrapping the artifact
// [Store]. It exposes the four tools as a [tool.ToolProvider], the
// `/artifacts` + `/attach` slash commands, snapshots per-session
// scope/workspace via InitState, and reaps a root's artifacts on
// root-session close ([extension.Closer]).
type Extension struct {
	store   *Store
	agentID string
	logger  *slog.Logger
}

// NewExtension constructs the artifact extension over store. agentID
// signs the ExtensionFrames it emits on publish / upload.
func NewExtension(store *Store, agentID string, logger *slog.Logger) *Extension {
	if logger == nil {
		logger = slog.Default()
	}
	return &Extension{store: store, agentID: agentID, logger: logger}
}

// Ingest registers a host file into a root conversation's artifact
// store and returns its ref — the adapter-facing UPLOAD entry (the
// user attached a file; the adapter wrote it to a temp/host path and
// hands the PATH here, bytes out-of-band). Uploads overwrite in place
// (re-attaching the same name replaces). The caller emits the
// [OpUploaded] frame (a slash handler returns it; a remote adapter
// routes it through the session manager).
func (e *Extension) Ingest(rootID, srcPath, name string) (protocol.ArtifactRef, error) {
	return e.store.Register(rootID, srcPath, name, "", true)
}

// emit builds + persists an artifact ExtensionFrame on the calling
// session's stream. Best-effort: a failure is logged, not surfaced.
func (e *Extension) emit(ctx context.Context, state extension.SessionState, op string, ref protocol.ArtifactRef) {
	data, err := json.Marshal(ref)
	if err != nil {
		return
	}
	frame := protocol.NewExtensionFrame(state.SessionID(), extension.AgentParticipant(e.agentID),
		providerName, protocol.CategoryMarker, op, data)
	if eerr := state.Emit(ctx, frame); eerr != nil {
		e.logger.Debug("artifact: emit frame failed", "op", op, "err", eerr)
	}
}

// Store returns the underlying folder store so out-of-band consumers
// (the adapter's upload Ingest / download Path, the retention sweep)
// can reach it without a session. Phase 8 step 4.
func (e *Extension) Store() *Store { return e.store }

// Compile-time interface assertions.
var (
	_ extension.Extension        = (*Extension)(nil)
	_ extension.StateInitializer = (*Extension)(nil)
	_ extension.Closer           = (*Extension)(nil)
	_ tool.ToolProvider          = (*Extension)(nil)
)

func (e *Extension) Name() string            { return providerName }
func (e *Extension) Lifetime() tool.Lifetime { return tool.LifetimePerAgent }

// sessionArtifacts is the per-session handle: the root scope key, the
// session's workspace dir (copy target / publish source root), and the
// overwrite guard (set once the session has read artifact:list).
type sessionArtifacts struct {
	rootID       string
	workspaceDir string
	listed       atomic.Bool
}

// InitState implements [extension.StateInitializer]. Snapshots the
// root scope (parent-chain walk to depth 0) and the calling session's
// workspace dir. Registered AFTER the workspace extension so
// workspace.FromState is populated by the time this runs.
func (e *Extension) InitState(_ context.Context, state extension.SessionState) error {
	wsDir := ""
	if h := wsext.FromState(state); h != nil {
		wsDir = h.Dir()
	}
	state.SetValue(StateKey, &sessionArtifacts{
		rootID:       walkToRootID(state),
		workspaceDir: wsDir,
	})
	return nil
}

// FromState returns the per-session handle, or nil when InitState
// hasn't run for this session.
func FromState(state extension.SessionState) *sessionArtifacts {
	v, ok := state.Value(StateKey)
	if !ok {
		return nil
	}
	sa, _ := v.(*sessionArtifacts)
	return sa
}

// CloseSession implements [extension.Closer]. Deletes the root
// conversation's artifacts ONLY on ROOT-session close (depth 0); a
// mission / worker close never deletes — artifacts belong to the root
// (design 007 §7). Idle / quota retention is the periodic sweep's job.
func (e *Extension) CloseSession(_ context.Context, state extension.SessionState) error {
	if state.Depth() != 0 {
		return nil
	}
	if err := e.store.ReapRoot(state.SessionID()); err != nil {
		e.logger.Warn("artifact: reap on root close failed", "root", state.SessionID(), "err", err)
	}
	return nil
}

// walkToRootID returns the root (depth-0) ancestor's session id — the
// artifact scope key shared by every mission / worker in the
// conversation.
func walkToRootID(state extension.SessionState) string {
	cur := state
	for cur.Depth() > 0 {
		p, ok := cur.Parent()
		if !ok {
			break
		}
		cur = p
	}
	return cur.SessionID()
}

// ---------- ToolProvider surface ----------

const listSchema = `{"type":"object","properties":{}}`

const copySchema = `{
  "type": "object",
  "properties": {
    "id":   {"type": "string", "description": "Artifact id (from artifact:list) to copy into your workspace."},
    "path": {"type": "string", "description": "Optional workspace-relative destination path. Default: the artifact id at the workspace root. Absolute paths and \"..\" escapes are rejected."}
  },
  "required": ["id"]
}`

const publishSchema = `{
  "type": "object",
  "properties": {
    "path":      {"type": "string", "description": "Workspace-relative path of the file to publish. Absolute paths and \"..\" escapes are rejected."},
    "name":      {"type": "string", "description": "Optional artifact name; defaults to the file's basename. Sanitized to a path/URL-safe id."},
    "type":      {"type": "string", "description": "Optional content-type hint; the store otherwise sniffs it."},
    "overwrite": {"type": "boolean", "description": "Replace an existing same-named artifact. Default false; requires having read artifact:list first."}
  },
  "required": ["path"]
}`

const deleteSchema = `{
  "type": "object",
  "properties": {
    "id": {"type": "string", "description": "Artifact id (from artifact:list) to delete."}
  },
  "required": ["id"]
}`

// List implements [tool.ToolProvider].
func (e *Extension) List(_ context.Context) ([]tool.Tool, error) {
	return []tool.Tool{
		{
			Name:             providerName + ":list",
			Description:      "List the conversation's artifacts (the user's uploads + everything published this conversation): id · name · type · size. Read this before overwriting an existing artifact.",
			Provider:         providerName,
			PermissionObject: PermList,
			ArgSchema:        json.RawMessage(listSchema),
		},
		{
			Name:             providerName + ":copy",
			Description:      "Copy an artifact into your workspace as a normal local file so you can read / process it. Returns the workspace-relative path.",
			Provider:         providerName,
			PermissionObject: PermCopy,
			ArgSchema:        json.RawMessage(copySchema),
		},
		{
			Name:             providerName + ":publish",
			Description:      "Publish a workspace file as a durable artifact the user can download. Use this for any deliverable / result the user asked for. Non-overwriting by default.",
			Provider:         providerName,
			PermissionObject: PermPublish,
			ArgSchema:        json.RawMessage(publishSchema),
		},
		{
			Name:             providerName + ":delete",
			Description:      "Delete an artifact from the conversation's store.",
			Provider:         providerName,
			PermissionObject: PermDelete,
			ArgSchema:        json.RawMessage(deleteSchema),
		},
	}, nil
}

// Call implements [tool.ToolProvider].
func (e *Extension) Call(ctx context.Context, name string, args json.RawMessage) (json.RawMessage, error) {
	switch stripProviderPrefix(name) {
	case "list":
		return e.callList(ctx)
	case "copy":
		return e.callCopy(ctx, args)
	case "publish":
		return e.callPublish(ctx, args)
	case "delete":
		return e.callDelete(ctx, args)
	default:
		return nil, fmt.Errorf("%w: artifact:%s", tool.ErrUnknownTool, stripProviderPrefix(name))
	}
}

// Subscribe / Close — stateless provider surface.
func (e *Extension) Subscribe(_ context.Context) (<-chan tool.ProviderEvent, error) { return nil, nil }
func (e *Extension) Close() error                                                   { return nil }

func stripProviderPrefix(name string) string {
	pfx := providerName + ":"
	if strings.HasPrefix(name, pfx) {
		return name[len(pfx):]
	}
	return name
}

// ---------- tool-dispatch handlers ----------

type toolError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type toolErrorResponse struct {
	Error toolError `json:"error"`
}

func toolErr(code, msg string) (json.RawMessage, error) {
	return json.Marshal(toolErrorResponse{Error: toolError{Code: code, Message: msg}})
}

// fromCtx resolves the calling session's artifact handle.
func (e *Extension) fromCtx(ctx context.Context) (*sessionArtifacts, json.RawMessage, error) {
	state, ok := extension.SessionStateFromContext(ctx)
	if !ok {
		out, err := toolErr("session_gone", "no session attached to dispatch ctx")
		return nil, out, err
	}
	sa := FromState(state)
	if sa == nil {
		out, err := toolErr("unavailable", "artifact extension state not initialised")
		return nil, out, err
	}
	return sa, nil, nil
}

func (e *Extension) callList(ctx context.Context) (json.RawMessage, error) {
	sa, errOut, err := e.fromCtx(ctx)
	if sa == nil {
		return errOut, err
	}
	refs, lerr := e.store.List(sa.rootID)
	if lerr != nil {
		return toolErr("io", lerr.Error())
	}
	sa.listed.Store(true) // arms the overwrite guard
	return json.Marshal(map[string]any{"artifacts": refs})
}

type copyInput struct {
	ID   string `json:"id"`
	Path string `json:"path"`
}

func (e *Extension) callCopy(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
	sa, errOut, err := e.fromCtx(ctx)
	if sa == nil {
		return errOut, err
	}
	var in copyInput
	if uerr := json.Unmarshal(args, &in); uerr != nil {
		return toolErr("bad_request", fmt.Sprintf("invalid artifact:copy args: %v", uerr))
	}
	if in.ID == "" {
		return toolErr("bad_request", "id is required")
	}
	if sa.workspaceDir == "" {
		return toolErr("unavailable", "no workspace to copy into")
	}
	rel := in.Path
	if rel == "" {
		rel = in.ID
	}
	dest, derr := confineWorkspace(sa.workspaceDir, rel)
	if derr != nil {
		return toolErr("bad_request", derr.Error())
	}
	if _, cerr := e.store.Copy(sa.rootID, in.ID, dest); cerr != nil {
		if errors.Is(cerr, ErrNotFound) {
			return toolErr("not_found", cerr.Error())
		}
		return toolErr("io", cerr.Error())
	}
	return json.Marshal(map[string]string{"path": filepath.Clean(rel)})
}

type publishInput struct {
	Path      string `json:"path"`
	Name      string `json:"name"`
	Type      string `json:"type"`
	Overwrite bool   `json:"overwrite"`
}

func (e *Extension) callPublish(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
	sa, errOut, err := e.fromCtx(ctx)
	if sa == nil {
		return errOut, err
	}
	var in publishInput
	if uerr := json.Unmarshal(args, &in); uerr != nil {
		return toolErr("bad_request", fmt.Sprintf("invalid artifact:publish args: %v", uerr))
	}
	if in.Path == "" {
		return toolErr("bad_request", "path is required")
	}
	if sa.workspaceDir == "" {
		return toolErr("unavailable", "no workspace to publish from")
	}
	if in.Overwrite && !sa.listed.Load() {
		return toolErr("read_list_first", "read the artifact list (artifact:list) before overwriting an existing artifact")
	}
	src, serr := confineWorkspace(sa.workspaceDir, in.Path)
	if serr != nil {
		return toolErr("bad_request", serr.Error())
	}
	ref, rerr := e.store.Register(sa.rootID, src, in.Name, in.Type, in.Overwrite)
	if rerr != nil {
		switch {
		case errors.Is(rerr, ErrExists):
			return toolErr("exists", rerr.Error()+" — read artifact:list, then set overwrite:true to replace")
		case errors.Is(rerr, ErrQuota):
			return toolErr("quota", rerr.Error())
		case errors.Is(rerr, ErrNotFound):
			return toolErr("not_found", rerr.Error())
		case errors.Is(rerr, ErrBadName):
			return toolErr("bad_request", rerr.Error())
		default:
			return toolErr("io", rerr.Error())
		}
	}
	// Announce the publish so the adapter can render an open/download
	// element (refs only — bytes stay out-of-band).
	if state, ok := extension.SessionStateFromContext(ctx); ok {
		e.emit(ctx, state, OpProduced, ref)
	}
	resp := map[string]any{"artifact": ref}
	if in.Overwrite {
		resp["note"] = "overwrote an existing artifact — review artifact:list"
	}
	return json.Marshal(resp)
}

type deleteInput struct {
	ID string `json:"id"`
}

func (e *Extension) callDelete(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
	sa, errOut, err := e.fromCtx(ctx)
	if sa == nil {
		return errOut, err
	}
	var in deleteInput
	if uerr := json.Unmarshal(args, &in); uerr != nil {
		return toolErr("bad_request", fmt.Sprintf("invalid artifact:delete args: %v", uerr))
	}
	if in.ID == "" {
		return toolErr("bad_request", "id is required")
	}
	if derr := e.store.Delete(sa.rootID, in.ID); derr != nil {
		if errors.Is(derr, ErrNotFound) {
			return toolErr("not_found", derr.Error())
		}
		return toolErr("io", derr.Error())
	}
	return json.Marshal(map[string]bool{"deleted": true})
}

// confineWorkspace joins a workspace-relative path under wsDir,
// rejecting absolute paths and ".." escapes — the same boundary
// bash-mcp / python-mcp enforce on the session workspace.
func confineWorkspace(wsDir, rel string) (string, error) {
	if filepath.IsAbs(rel) {
		return "", fmt.Errorf("path must be relative to the workspace (no absolute paths)")
	}
	cleaned := filepath.Clean(rel)
	if cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return "", fmt.Errorf("path escapes the workspace")
	}
	return filepath.Join(wsDir, cleaned), nil
}
