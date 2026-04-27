package local

// Config is the local-DB section of the agent config. Populated by
// pkg/config when cfg.LocalDBEnabled is true. pkg/config composes this
// type into Config.LocalDB; pkg/store/local itself does not import
// pkg/config, which keeps the dependency direction one-way
// (config → local, never local → config).
type Config struct {
	// DB holds the CoreDB (engine.db) path and pool settings.
	DB DBConfig `mapstructure:"db"`

	// MemoryPath is the provisioned hub.db location that is attached
	// as the "hub.db" RuntimeSource.
	MemoryPath string `mapstructure:"memory_path"`

	// Models registers additional data sources in the engine (llm-*
	// and embedding types). local.New hands each entry to
	// engine.RegisterDataSource as-is. Filtering / routing is not a
	// concern of this package.
	Models []ModelDef `mapstructure:"models"`
}

// DBConfig configures the CoreDB file and connection pool.
type DBConfig struct {
	Path     string     `mapstructure:"path"`
	Settings DBSettings `mapstructure:"settings"`
}

// DBSettings mirrors query-engine/pkg/db.Settings — duplicated here to
// keep pkg/store/local independent of engine-internal types (Viper
// decodes directly into this struct).
type DBSettings struct {
	Timezone      string `mapstructure:"timezone"`
	HomeDirectory string `mapstructure:"home_directory"`
	MaxMemory     int    `mapstructure:"max_memory"`
	WorkerThreads int    `mapstructure:"worker_threads"`
}

// ModelDef declares one data source (llm-* or embedding) to register
// in the embedded hugr engine. Path is URL-shaped; ${ENV_VAR}
// references are expanded at attach time.
type ModelDef struct {
	Name string `mapstructure:"name"`
	Type string `mapstructure:"type"` // llm-openai | llm-anthropic | llm-gemini | embedding
	Path string `mapstructure:"path"`
}

// Identity identifies the running agent instance. Declared here
// (rather than in pkg/config) so pkg/config can compose local.Config
// without inducing a cycle through Identity back into pkg/config.
type Identity struct {
	ID      string `mapstructure:"id"`
	ShortID string `mapstructure:"short_id"`
	Name    string `mapstructure:"name"`
	Type    string `mapstructure:"type"`
}

// EmbeddingConfig is the agent's embedding setup (model name, vector
// dimension, and whether to register locally or defer to remote Hugr).
// Declared in pkg/store/local for the same cycle-avoidance reason as
// Identity. Used by local.New (for probe + source registration) and
// by pkg/store consumers (for vector search dim).
type EmbeddingConfig struct {
	Mode      string `mapstructure:"mode"` // local | hugr
	Model     string `mapstructure:"model"`
	Dimension int    `mapstructure:"dimension"`
}
