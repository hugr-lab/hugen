package config

// LocalConfig is the local-DB section of the agent config — file
// paths, pool settings, model registrations. Owned by pkg/config;
// pkg/store/local consumes it through LocalView.
type LocalConfig struct {
	// DB holds the CoreDB (engine.db) path and pool settings.
	DB DBConfig `mapstructure:"db"`

	// MemoryPath is the provisioned hub.db location that is attached
	// as the "hub.db" RuntimeSource.
	MemoryPath string `mapstructure:"memory_path"`

	// Models registers additional data sources in the engine (llm-*
	// and embedding types). pkg/store/local hands each entry to
	// engine.RegisterDataSource as-is.
	Models []ModelDef `mapstructure:"models"`
}

// DBConfig configures the CoreDB file and connection pool.
type DBConfig struct {
	Path     string     `mapstructure:"path"`
	Settings DBSettings `mapstructure:"settings"`
}

// DBSettings mirrors query-engine/pkg/db.Settings — duplicated here
// to keep pkg/store/local independent of engine-internal types.
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

// AgentIdentity identifies the running agent instance. Lives in
// pkg/config so every consumer (pkg/store/local for hub.db identity,
// pkg/auth/perm for template substitution) reads the same shape.
type AgentIdentity struct {
	ID      string `mapstructure:"id"`
	ShortID string `mapstructure:"short_id"`
	Name    string `mapstructure:"name"`
	Type    string `mapstructure:"type"`
}

// EmbeddingConfig is the agent's embedding setup (model name, vector
// dimension, and whether to register locally or defer to remote
// Hugr). Used by pkg/store/local for source registration and by
// memory-pipeline consumers for vector-search dim.
type EmbeddingConfig struct {
	Mode      string `mapstructure:"mode"` // local | hugr
	Model     string `mapstructure:"model"`
	Dimension int    `mapstructure:"dimension"`
}
