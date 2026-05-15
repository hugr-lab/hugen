package tui

import (
	"testing"
)

func TestResolveTheme_ExplicitOverridePicksMatch(t *testing.T) {
	cases := []struct {
		override string
		want     string
	}{
		{"dark", "dark"},
		{"light", "light"},
		{"DARK", "dark"},
		{"  Light  ", "light"},
	}
	for _, c := range cases {
		t.Run(c.override, func(t *testing.T) {
			got := resolveTheme(c.override).Name
			if got != c.want {
				t.Errorf("resolveTheme(%q).Name = %q; want %q", c.override, got, c.want)
			}
		})
	}
}

func TestResolveTheme_AutoUsesColorFGBG(t *testing.T) {
	cases := []struct {
		env  string
		want string
	}{
		{"15;0", "dark"},  // white fg on black bg → operator sees dark term
		{"0;15", "light"}, // black fg on white bg → light term
		{"7;0", "light"},  // fg=7 (lower half) → light per the spec
		{"", "dark"},       // missing var → dark default
		{"garbage", "dark"}, // unparseable → dark default
	}
	for _, c := range cases {
		t.Run(c.env, func(t *testing.T) {
			t.Setenv("COLORFGBG", c.env)
			got := resolveTheme("").Name
			if got != c.want {
				t.Errorf("COLORFGBG=%q: got %q want %q", c.env, got, c.want)
			}
		})
	}
}

func TestResolveTheme_ExplicitOverridesAutoDetect(t *testing.T) {
	t.Setenv("COLORFGBG", "0;15") // would auto-pick light
	if got := resolveTheme("dark").Name; got != "dark" {
		t.Errorf("explicit dark with light env: got %q want dark", got)
	}
}

func TestApplyTheme_SwitchesAndRestores(t *testing.T) {
	prev := applyTheme(lightTheme)
	if activeTheme.Name != "light" {
		t.Errorf("activeTheme = %q after applyTheme(light); want light", activeTheme.Name)
	}
	applyTheme(prev)
	if activeTheme.Name != prev.Name {
		t.Errorf("restore failed: active=%s want=%s", activeTheme.Name, prev.Name)
	}
}
