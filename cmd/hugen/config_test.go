package main

import "testing"

func TestBootstrapConfig_IdentityMode(t *testing.T) {
	cases := []struct {
		name string
		cfg  *BootstrapConfig
		want string
	}{
		{
			name: "local mode is autonomous-agent",
			cfg:  &BootstrapConfig{Mode: "local"},
			want: "autonomous-agent",
		},
		{
			name: "remote mode is personal-assistant",
			cfg:  &BootstrapConfig{Mode: "remote"},
			want: "personal-assistant",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.cfg.IdentityMode(); got != tc.want {
				t.Errorf("IdentityMode() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestBootstrapConfig_LocalOIDCEnabled(t *testing.T) {
	cases := []struct {
		name string
		cfg  *BootstrapConfig
		want bool
	}{
		{
			name: "OIDC needed: local mode with hugr URL but no static token",
			cfg: &BootstrapConfig{
				Mode: "local",
				Hugr: HugrConfig{URL: "http://hugr"},
			},
			want: true,
		},
		{
			name: "OIDC not needed: remote mode (uses static tokens)",
			cfg: &BootstrapConfig{
				Mode: "remote",
				Hugr: HugrConfig{URL: "http://hugr", AccessToken: "x", TokenURL: "http://t"},
			},
			want: false,
		},
		{
			name: "OIDC not needed: no hugr URL",
			cfg:  &BootstrapConfig{Mode: "local"},
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.cfg.LocalOIDCEnabled(); got != tc.want {
				t.Errorf("LocalOIDCEnabled() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestRun_RefusesDeferredSubcommands(t *testing.T) {
	cases := []struct {
		name     string
		args     []string
		wantExit int
		wantOut  string
	}{
		{"a2a refused", []string{"a2a"}, exitUsage, "phase 10"},
		{"unknown sub refused", []string{"wat"}, exitUsage, "unknown subcommand"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := &strBuf{}
			got := run(tc.args, out)
			if got != tc.wantExit {
				t.Errorf("exit = %d, want %d", got, tc.wantExit)
			}
			if !contains(out.s, tc.wantOut) {
				t.Errorf("output %q does not contain %q", out.s, tc.wantOut)
			}
		})
	}
}

type strBuf struct{ s string }

func (b *strBuf) Write(p []byte) (int, error) {
	b.s += string(p)
	return len(p), nil
}

func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
