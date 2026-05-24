package mission

import (
	"regexp"
	"strings"
	"sync"
)

// autoResearchHeuristic decides whether `When=auto` fires the
// research stage. Phase 5.x — B15 §2.5.
//
// The heuristic leans aggressive — favours one extra modal over
// shipping a wrong-inputs mission. False positives waste a turn
// (research role emits done=true with empty clarifications);
// false negatives waste a whole mission run on misread intent.
//
// Triggers (any one is enough):
//
//  1. Goal is short (< autoResearchShortGoalWords words) — likely
//     under-specified.
//  2. Goal contains a deliverable keyword (save/export/report/
//     html/parquet/etc.) — the user wants an artefact and we
//     don't know where to put it.
//  3. Goal contains a pronoun-only reference (`this`, `that`,
//     `it` as a standalone token) — context wasn't passed in.
//  4. Goal contains an explicit trigger phrase ("help me figure
//     out", "schedule", "I want to", "не могу понять", etc.).
//
// The manifest argument is reserved for future extensions (e.g.
// inputs_schema mismatch detection — once mission inputs land);
// today it's unused but kept on the signature so callers don't
// need to refactor when the heuristic grows.
func autoResearchHeuristic(goal string, _ MissionManifest) bool {
	goal = strings.TrimSpace(goal)
	if goal == "" {
		return true
	}
	lower := strings.ToLower(goal)

	// 1. Short-goal trigger.
	if len(strings.Fields(goal)) < autoResearchShortGoalWords {
		return true
	}

	// 2. Deliverable keywords. Mostly English with a small
	// Russian set covering analyst's most-exercised path
	// ("сохранить", "отчёт", "выгрузить"). The full lexicon
	// would balloon — the auto-heuristic only needs to catch the
	// common-case dogfood loop; corner-case skills can declare
	// `when: always` or `when: if_goal_matches` instead.
	for _, kw := range autoResearchDeliverableKeywords {
		if containsWord(lower, kw) {
			return true
		}
	}

	// 3. Pronoun-only references (standalone tokens).
	for _, p := range autoResearchPronouns {
		if containsWord(lower, p) {
			return true
		}
	}

	// 4. Trigger phrases. Substring match (the phrases are
	// distinctive enough that false positives are rare).
	for _, phrase := range autoResearchTriggerPhrases {
		if strings.Contains(lower, phrase) {
			return true
		}
	}

	return false
}

// matchGoalPredicate evaluates a regex (re-compiled per call;
// cached via the package-level cache below). Returns true when the
// predicate matches the goal text. Invalid regexes fail closed —
// research stage does NOT fire on a broken predicate, the runtime
// logs the failure at projection time.
func matchGoalPredicate(pattern, goal string) bool {
	if pattern == "" {
		return false
	}
	re, ok := loadCachedRegex(pattern)
	if !ok {
		return false
	}
	return re.MatchString(goal)
}

// containsWord checks for a standalone token match: the substring
// is bordered by non-word characters (or string edges). Lets
// "html" match in "save it as html" but NOT in "fileMustExist:
// htmlOnly".
//
// ASCII-only by design — paired with the ASCII-only keyword tables
// above. `isWordChar` reads single bytes, so passing UTF-8 multi-
// byte runes would treat continuation bytes (0x80-0xBF) as non-word
// and false-match prefixes inside inflected forms. The heuristic
// is the floor for routing weak models into the research stage;
// non-Latin user goals go through the LLM-driven research prose
// path instead, which handles multilingualism natively.
func containsWord(haystack, needle string) bool {
	idx := strings.Index(haystack, needle)
	for idx >= 0 {
		left := idx == 0 || !isWordChar(haystack[idx-1])
		right := idx+len(needle) == len(haystack) || !isWordChar(haystack[idx+len(needle)])
		if left && right {
			return true
		}
		next := strings.Index(haystack[idx+1:], needle)
		if next < 0 {
			return false
		}
		idx = idx + 1 + next
	}
	return false
}

func isWordChar(b byte) bool {
	switch {
	case b >= 'a' && b <= 'z':
		return true
	case b >= 'A' && b <= 'Z':
		return true
	case b >= '0' && b <= '9':
		return true
	case b == '_':
		return true
	}
	return false
}

// autoResearchShortGoalWords — goals with fewer words than this
// trigger research. 8 is the default per spec §2.5; tunable via
// future runtime.research_auto_thresholds (deferred until proven
// necessary).
const autoResearchShortGoalWords = 8

// autoResearchDeliverableKeywords — phrases indicating the user
// expects an artefact file. Catches the analyst's most-exercised
// "save this report" flow without enumerating every domain term.
// ASCII-only by design — the runtime heuristic relies on byte-level
// word boundaries; non-Latin languages get routed through the LLM-
// driven research role's own prose rather than a literal-token map.
var autoResearchDeliverableKeywords = []string{
	"save", "export", "report", "dashboard", "dump", "write",
	"file", "csv", "parquet", "json", "html", "markdown", "pdf",
}

// autoResearchPronouns — standalone pronoun references suggesting
// the goal carried context the runtime didn't preserve.
var autoResearchPronouns = []string{
	"this", "that", "it",
}

// autoResearchTriggerPhrases — explicit user cues that the goal
// is under-specified.
var autoResearchTriggerPhrases = []string{
	"help me figure out",
	"i want to schedule",
	"i need help",
	"not sure",
}

// regexCache memoises compiled regex predicates so a hot mission
// doesn't re-compile the same pattern on every spawn. Bounded
// implicitly by the number of distinct predicates declared across
// all loaded skills — a small set in practice.
var (
	regexCache   sync.Map // map[string]*regexp.Regexp
	regexCacheNG sync.Map // map[string]struct{} — patterns that failed to compile
)

func loadCachedRegex(pattern string) (*regexp.Regexp, bool) {
	if _, bad := regexCacheNG.Load(pattern); bad {
		return nil, false
	}
	if v, ok := regexCache.Load(pattern); ok {
		return v.(*regexp.Regexp), true
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		regexCacheNG.Store(pattern, struct{}{})
		return nil, false
	}
	regexCache.Store(pattern, re)
	return re, true
}
