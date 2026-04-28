// Package console wires stdin/stdout into the runtime as a chat
// adapter. Lines starting with `/` become SlashCommand Frames;
// everything else becomes UserMessage.
package console

import (
	"strings"
	"unicode"
)

// ParsedCommand is the tokenised form of a slash-command line.
type ParsedCommand struct {
	Name string
	Args []string
	Raw  string
}

// IsSlashCommand reports whether a line starts with `/<name>` where
// name matches [a-z][a-z0-9_-]*. Lines that look like `/` (no name)
// or `//` fall through.
func IsSlashCommand(line string) bool {
	t := strings.TrimSpace(line)
	if len(t) < 2 || t[0] != '/' {
		return false
	}
	first := rune(t[1])
	if !((first >= 'a' && first <= 'z') || (first >= 'A' && first <= 'Z')) {
		return false
	}
	return true
}

// ParseSlashCommand tokenises a slash-command line. Whitespace is
// the delimiter; double-quoted segments preserve spaces. Returns the
// command name (lowercased), the arg slice (which may be nil), and
// the original line as Raw.
func ParseSlashCommand(line string) ParsedCommand {
	raw := strings.TrimRight(line, "\r\n")
	t := strings.TrimSpace(raw)
	t = strings.TrimPrefix(t, "/")
	tokens := tokenise(t)
	if len(tokens) == 0 {
		return ParsedCommand{Raw: raw}
	}
	return ParsedCommand{
		Name: strings.ToLower(tokens[0]),
		Args: tokens[1:],
		Raw:  raw,
	}
}

// tokenise splits a string by whitespace, respecting "..."-quoted
// segments. Backslash escapes are not honoured (KISS).
func tokenise(s string) []string {
	var out []string
	var cur strings.Builder
	inQuote := false
	for _, r := range s {
		switch {
		case r == '"':
			inQuote = !inQuote
		case unicode.IsSpace(r) && !inQuote:
			if cur.Len() > 0 {
				out = append(out, cur.String())
				cur.Reset()
			}
		default:
			cur.WriteRune(r)
		}
	}
	if cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out
}
