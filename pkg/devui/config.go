package devui

// Config is the DevUI listener YAML section (`devui:` in config.yaml).
// Loopback-only by construction: the listener binds 127.0.0.1 + Port.
// BaseURL is used to wire the webui SPA's redirects.
type Config struct {
	Port    int    `mapstructure:"port"`
	BaseURL string `mapstructure:"base_url"`
}
