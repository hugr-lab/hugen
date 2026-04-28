package console

import "testing"

func TestParseSlashCommand(t *testing.T) {
	cases := []struct {
		name     string
		in       string
		isSlash  bool
		wantName string
		wantArgs []string
	}{
		{"plain text", "hello world", false, "", nil},
		{"empty", "", false, "", nil},
		{"single slash", "/", false, "", nil},
		{"double slash", "//", false, "", nil},
		{"help", "/help", true, "help", nil},
		{"help_with_space", "  /help  ", true, "help", nil},
		{"note", "/note hello world", true, "note", []string{"hello", "world"}},
		{"note_quoted", `/note "hello world"`, true, "note", []string{"hello world"}},
		{"model_use", "/model use cheap", true, "model", []string{"use", "cheap"}},
		{"model_use_provider", `/model use "openai/gpt-5"`, true, "model", []string{"use", "openai/gpt-5"}},
		{"upper_case_normalised", "/Help", true, "help", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := IsSlashCommand(tc.in)
			if got != tc.isSlash {
				t.Errorf("IsSlashCommand: got %v, want %v", got, tc.isSlash)
			}
			if !tc.isSlash {
				return
			}
			pc := ParseSlashCommand(tc.in)
			if pc.Name != tc.wantName {
				t.Errorf("name: got %q, want %q", pc.Name, tc.wantName)
			}
			if len(pc.Args) != len(tc.wantArgs) {
				t.Fatalf("args len: got %v, want %v", pc.Args, tc.wantArgs)
			}
			for i := range tc.wantArgs {
				if pc.Args[i] != tc.wantArgs[i] {
					t.Errorf("arg[%d]: got %q, want %q", i, pc.Args[i], tc.wantArgs[i])
				}
			}
		})
	}
}
