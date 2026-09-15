package main

import (
	"path/filepath"
	"testing"

	"github.com/chenhg5/cc-connect/config"
)

func TestProviderControlLocationSupportsExistingDeployments(t *testing.T) {
	dir := t.TempDir()
	for _, tc := range []struct {
		name string
		cfg  config.ProviderControlConfig
		want string
		app  string
	}{
		{"explicit", config.ProviderControlConfig{ConfigFile: filepath.Join(dir, "control.json"), App: "codex"}, filepath.Join(dir, "control.json"), "codex"},
		{"legacy-database", config.ProviderControlConfig{DBPath: filepath.Join(dir, "cc-switch.db"), AppType: "codex"}, filepath.Join(dir, "control-api.json"), "codex"},
		{"legacy-proxy", config.ProviderControlConfig{ConfigFile: filepath.Join(dir, "control-api.json"), App: "claude", ProxyProvider: "cc-switch"}, filepath.Join(dir, "control-api.json"), "claude"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path, app, err := providerControlLocation(&tc.cfg)
			if err != nil || path != tc.want || app != tc.app {
				t.Fatalf("location=%q app=%q err=%v", path, app, err)
			}
		})
	}
	for _, cfg := range []config.ProviderControlConfig{{}, {App: "codex", AppType: "claude"}} {
		if _, _, err := providerControlLocation(&cfg); err == nil {
			t.Fatal("invalid configuration accepted")
		}
	}
	if err := configureProviderControl(nil, nil); err != nil {
		t.Fatal("absent integration must remain optional")
	}
}
