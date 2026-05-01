package main

import "testing"

func TestParseUVVersion(t *testing.T) {
	cases := []struct {
		name            string
		in              string
		wantOK          bool
		maj, min, patch int
	}{
		{"plain", "uv 0.4.0\n", true, 0, 4, 0},
		{"with rev", "uv 0.11.8 (deadbeef)\n", true, 0, 11, 8},
		{"prerelease", "uv 0.5.2rc1\n", true, 0, 5, 2},
		{"missing prefix", "0.4.0\n", false, 0, 0, 0},
		{"too short", "uv 0.4\n", false, 0, 0, 0},
		{"empty", "", false, 0, 0, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			maj, min, patch, ok := parseUVVersion(c.in)
			if ok != c.wantOK {
				t.Fatalf("ok = %v, want %v", ok, c.wantOK)
			}
			if !ok {
				return
			}
			if maj != c.maj || min != c.min || patch != c.patch {
				t.Fatalf("got %d.%d.%d, want %d.%d.%d",
					maj, min, patch, c.maj, c.min, c.patch)
			}
		})
	}
}

func TestStripNonDigits(t *testing.T) {
	cases := map[string]string{
		"42":      "42",
		"42rc1":   "42",
		"":        "",
		"rc":      "",
		"3a":      "3",
		"100b500": "100",
	}
	for in, want := range cases {
		if got := stripNonDigits(in); got != want {
			t.Errorf("stripNonDigits(%q) = %q, want %q", in, got, want)
		}
	}
}
