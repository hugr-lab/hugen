package config

import (
	"fmt"
	"os"
	"strings"

	"github.com/spf13/viper"

	"github.com/hugr-lab/hugen/pkg/a2a"
	"github.com/hugr-lab/hugen/pkg/devui"
	"github.com/hugr-lab/hugen/pkg/store/local"
)

// BootstrapConfig is the minimal set of fields needed before the
// hugr client can be constructed. All values come from .env — no
// YAML is read at this stage. Mode is inferred from whether the
// caller supplied HUGR_ACCESS_TOKEN + HUGR_TOKEN_URL:
//
//   - both set → remote mode: hub hands this process a token; full
//     config is pulled from hub.db.agents later.
//   - either missing → local mode: full config comes from
//     config.yaml; hugr auth falls back to OIDC discovery against
//     {HUGR_URL}/auth/config.
type BootstrapConfig struct {
	Hugr     HugrConfig
	HugrAuth AuthConfig     // single auth entry reserved for the hugr connection
	A2A      a2a.Config
	DevUI    devui.Config
	Identity local.Identity // populated by LoadLocal (YAML) or whoami (remote)
}

// Remote returns true when the operator supplied enough to run in
// remote-managed mode (RemoteStore-driven token + hub-sourced
// config). Otherwise we're local.
func (b *BootstrapConfig) Remote() bool {
	return b != nil &&
		b.HugrAuth.AccessToken != "" &&
		b.HugrAuth.TokenURL != ""
}

// LoadBootstrap reads only .env and returns the minimal config
// needed to spin up auth + hugr client. envPath is the .env file to
// read; pass "" to skip the file and rely entirely on OS env.
func LoadBootstrap(envPath string) (*BootstrapConfig, error) {
	v := viper.New()
	if envPath != "" {
		v.SetConfigFile(envPath)
		v.SetConfigType("env")
	}
	v.AutomaticEnv()

	v.SetDefault("HUGR_URL", "http://localhost:15000")
	v.SetDefault("AGENT_PORT", 10000)
	v.SetDefault("AGENT_DEVUI_PORT", 10001)

	_ = v.ReadInConfig()

	// Propagate .env values into the process environment so
	// os.ExpandEnv in config.yaml paths (${API_KEY}, ${LLM_URL}, …)
	// resolves them. Same behaviour as the legacy Load.
	for _, key := range v.AllKeys() {
		upper := strings.ToUpper(key)
		if _, set := os.LookupEnv(upper); set {
			continue
		}
		_ = os.Setenv(upper, v.GetString(key))
	}

	hugrURL := strings.TrimRight(v.GetString("HUGR_URL"), "/")
	if os.Getenv("HUGR_MCP_URL") == "" && hugrURL != "" {
		_ = os.Setenv("HUGR_MCP_URL", hugrURL+"/mcp")
	}

	port := v.GetInt("AGENT_PORT")
	devPort := v.GetInt("AGENT_DEVUI_PORT")

	boot := &BootstrapConfig{
		Hugr: HugrConfig{
			URL:    hugrURL,
			MCPUrl: hugrURL + "/mcp",
		},
		A2A: a2a.Config{
			Port:    port,
			BaseURL: baseURL(v.GetString("AGENT_BASE_URL"), port),
		},
		DevUI: devui.Config{
			Port:    devPort,
			BaseURL: baseURL(v.GetString("AGENT_DEVUI_BASE_URL"), devPort),
		},
		HugrAuth: AuthConfig{
			Name:        "hugr",
			Type:        "hugr",
			AccessToken: v.GetString("HUGR_ACCESS_TOKEN"),
			TokenURL:    v.GetString("HUGR_TOKEN_URL"),
		},
	}
	if boot.HugrAuth.AccessToken == "" && boot.HugrAuth.TokenURL != "" {
		return nil, fmt.Errorf("config: HUGR_TOKEN_URL set without HUGR_ACCESS_TOKEN")
	}
	if boot.HugrAuth.AccessToken != "" && boot.HugrAuth.TokenURL == "" {
		return nil, fmt.Errorf("config: HUGR_ACCESS_TOKEN set without HUGR_TOKEN_URL")
	}
	return boot, nil
}
