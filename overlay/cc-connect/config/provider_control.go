package config

// ProviderControlConfig enables the optional desktop proxy control bridge.
// DBPath and AppType preserve older deployments; the database is never written
// or used as the authoritative source of the desktop's current selection.
type ProviderControlConfig struct {
	ConfigFile    string `toml:"config_file,omitempty"`
	App           string `toml:"app,omitempty"`
	DBPath        string `toml:"db_path,omitempty"`
	AppType       string `toml:"app_type,omitempty"`
	ProxyProvider string `toml:"proxy_provider,omitempty"`
}
