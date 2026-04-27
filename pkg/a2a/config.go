package a2a

// Config is the A2A listener YAML section (`a2a:` in config.yaml).
// OIDC callbacks also land on this listener so the redirect_uri is
// stable across run modes (a2a / devui / console).
type Config struct {
	Port    int    `mapstructure:"port"`
	BaseURL string `mapstructure:"base_url"`
}
