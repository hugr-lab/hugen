package mission

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/hugr-lab/hugen/pkg/extension"
	wsext "github.com/hugr-lab/hugen/pkg/extension/workspace"
)

// missionFile is one entry in the worker-spawn mission-files index: a
// file the mission has ACTUALLY produced (filled, not a scaffold) under
// the shared mission working dir. The index is rendered once into the
// worker's spawn brief (decorateWaveTasks → worker_contract.tmpl) so a
// worker reads the real input set by path instead of re-discovering it.
// Phase B31.
type missionFile struct {
	// Path is relative to the mission dir (e.g. "research/data-model.md").
	Path string
	// Size is a short human byte size ("1.2 KB"); empty when unknown.
	Size string
}

// maxMissionFilesIndex caps the index so a runaway working dir can't
// bloat every worker's spawn brief; the overflow is summarised as a
// "+N more" line (collectMissionFiles) rather than silently dropped.
const maxMissionFilesIndex = 40

// htmlCommentRe strips `<!-- ... -->` scaffold-guidance comments; the
// (?s) flag makes `.` span newlines so multi-line comments are removed.
var htmlCommentRe = regexp.MustCompile(`(?s)<!--.*?-->`)

// placeholderTokenRe matches an unfilled `<placeholder>` scaffold token
// (mirrors check_research.py's heuristic — kept skill-agnostic so the
// runtime needn't know any one skill's scaffold layout).
var placeholderTokenRe = regexp.MustCompile(`<[^>\s][^>]*>`)

// missionFilesForState resolves the calling mission's working dir and
// returns its produced-file index, or nil when the session has no
// workspace dir (root / standalone sessions). Thin wrapper so callers
// don't take the workspace-extension dependency.
func missionFilesForState(state extension.SessionState) []missionFile {
	ws := wsext.FromState(state)
	if ws == nil {
		return nil
	}
	return collectMissionFiles(ws.Dir())
}

// collectMissionFiles walks the mission working dir and returns the
// files the mission has GENUINELY produced — the input set a worker
// should read before re-deriving. The filter is skill-agnostic (the
// runtime can't know any one skill's scaffold layout):
//
//   - text files (.md / .markdown / .txt) are listed only when they
//     carry real content beyond the scaffold. Comments, headings,
//     blockquotes, table-rule separators, and lines still holding a
//     `<placeholder>` token don't count — the same language- and
//     structure-agnostic test check_research.py uses to tell a filled
//     research file from an untouched skeleton. An empty or
//     scaffold-only markdown file is NOT advertised, so a worker never
//     chases a stub the research `before` hook merely seeded.
//   - every other file (data: parquet / json / csv / xlsx, rendered
//     html, scripts) is listed when non-empty — a data file is never a
//     scaffold.
//
// Hidden entries (dotfiles / dot-dirs like .git) are skipped. Results
// sort by path for a stable, cache-friendly render and cap at
// maxMissionFilesIndex with a trailing "+N more" overflow line. Returns
// nil for an empty or absent dir. Best-effort throughout: an unreadable
// subtree is skipped, never fatal — the index is a convenience, not a
// contract.
func collectMissionFiles(dir string) []missionFile {
	if strings.TrimSpace(dir) == "" {
		return nil
	}
	var out []missionFile
	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // skip unreadable subtrees rather than abort
		}
		name := d.Name()
		if path != dir && strings.HasPrefix(name, ".") {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		info, ierr := d.Info()
		if ierr != nil || info.Size() == 0 {
			return nil
		}
		if isScaffoldText(path, name) {
			return nil
		}
		rel, rerr := filepath.Rel(dir, path)
		if rerr != nil {
			rel = name
		}
		out = append(out, missionFile{Path: rel, Size: humanSize(info.Size())})
		return nil
	})
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	if len(out) > maxMissionFilesIndex {
		extra := len(out) - maxMissionFilesIndex
		out = out[:maxMissionFilesIndex]
		out = append(out, missionFile{Path: fmt.Sprintf("… +%d more file(s)", extra)})
	}
	return out
}

// isScaffoldText reports whether path is a TEXT file that still looks
// like an unfilled scaffold (fewer than 2 real-content lines). Non-text
// files always return false (a data file is never a scaffold). A read
// failure conservatively returns false so the file is still listed —
// better to over-list than hide a real artifact.
func isScaffoldText(path, name string) bool {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".md", ".markdown", ".txt":
	default:
		return false
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	return realContentLines(string(b)) < 2
}

// realContentLines counts the lines a human actually wrote: HTML
// comments are stripped, then blank lines, heading / blockquote lines,
// pure table-rule separators, and lines still carrying a
// `<placeholder>` token are all ignored. A direct port of
// check_research.py's language-agnostic heuristic, so the runtime's
// "is this filled?" test matches the research gate's exactly. Phase B31.
func realContentLines(text string) int {
	text = htmlCommentRe.ReplaceAllString(text, "")
	n := 0
	for _, raw := range strings.Split(text, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "#") || strings.HasPrefix(line, ">") {
			continue
		}
		if strings.Trim(line, "|-: ") == "" { // table rule / separator
			continue
		}
		if placeholderTokenRe.MatchString(line) { // still a scaffold placeholder
			continue
		}
		n++
	}
	return n
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
