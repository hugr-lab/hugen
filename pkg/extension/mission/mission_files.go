package mission

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/hugr-lab/hugen/pkg/extension"
	wsext "github.com/hugr-lab/hugen/pkg/extension/workspace"
)

// missionFile is one entry in the worker-spawn mission-files index: a
// file the mission has produced under the shared working dir, surfaced
// to a worker by path (decorateWaveTasks → worker_contract.tmpl) so it
// reads the real input set instead of re-discovering it. Phase B31.
type missionFile struct {
	// Path is relative to the mission dir (e.g. "research/data-model.md").
	Path string
	// Size is a short human byte size ("1.2 KB"); empty when unknown.
	Size string
}

// missionFilesForState returns the index of files the mission has
// DECLARED it produced, for the worker spawn brief. The set is
// skill-agnostic by construction: the runtime lists only paths that a
// role or the runtime itself declared, and NEVER inspects file content
// to decide what counts — judging "filled vs scaffold" from content
// would bake one skill's template/scaffold convention (a specific
// markdown placeholder format, a JSON skeleton shape, …) into the
// universal runtime. Sources:
//
//   - spec.md — the mission contract the runtime writes itself.
//   - research artifacts — the relative paths the research role declared
//     in its handoff `file_refs` (a universal handoff field). The
//     research `check` gate already enforced the load-bearing files were
//     filled before that handoff was accepted, so the declaration is
//     trustworthy; the runtime needn't re-judge it.
//
// Returns nil when the session has no workspace dir (root / standalone)
// or nothing has been produced yet. Worker-produced data files reach a
// downstream worker through `depends_on` resolution (the handoff body is
// inlined), not this index.
func missionFilesForState(state extension.SessionState) []missionFile {
	ws := wsext.FromState(state)
	if ws == nil || ws.Dir() == "" {
		return nil
	}
	var refs []string
	if m := FromState(state); m != nil {
		refs = m.ResearchFileRefs()
	}
	return collectDeclaredFiles(ws.Dir(), refs)
}

// collectDeclaredFiles stats the runtime-known contract file (spec.md)
// plus each role-declared relative path, returning those that exist as a
// non-empty regular file under dir. Pure (dir + refs in, no SessionState)
// so it is unit-testable without a session fixture. Declared paths are
// normalised and confined to the mission dir — an absolute path or one
// escaping via `../` is rejected, so a buggy role can't point the index
// outside the workspace. De-dupes; results sort by path for a stable,
// cache-friendly render.
func collectDeclaredFiles(dir string, refs []string) []missionFile {
	if strings.TrimSpace(dir) == "" {
		return nil
	}
	seen := make(map[string]struct{})
	var rels []string
	add := func(rel string) {
		rel = filepath.ToSlash(strings.TrimSpace(rel))
		rel = strings.TrimPrefix(rel, "./")
		if rel == "" || filepath.IsAbs(rel) || rel == ".." || strings.HasPrefix(rel, "../") {
			return // keep the index inside the mission dir
		}
		if _, dup := seen[rel]; dup {
			return
		}
		seen[rel] = struct{}{}
		rels = append(rels, rel)
	}
	add("spec.md") // the contract the runtime authors
	for _, r := range refs {
		add(r)
	}

	out := make([]missionFile, 0, len(rels))
	for _, rel := range rels {
		info, err := os.Stat(filepath.Join(dir, filepath.FromSlash(rel)))
		if err != nil || info.IsDir() || info.Size() == 0 {
			continue // declared-but-absent / empty / a dir → not a usable artifact
		}
		out = append(out, missionFile{Path: rel, Size: humanSize(info.Size())})
	}
	if len(out) == 0 {
		return nil
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// humanSize renders a byte count as a short human string ("1.2 KB").
// Empty for non-positive sizes.
func humanSize(n int64) string {
	if n <= 0 {
		return ""
	}
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for x := n / unit; x >= unit; x /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}
