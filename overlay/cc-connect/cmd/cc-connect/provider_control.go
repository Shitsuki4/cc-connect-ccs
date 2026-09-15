package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/chenhg5/cc-connect/config"
	"github.com/chenhg5/cc-connect/core"
)

func configureProviderControl(engine *core.Engine, cfg *config.ProviderControlConfig) error {
	if cfg == nil {
		return nil
	}
	path, app, err := providerControlLocation(cfg)
	if err != nil {
		return err
	}
	control, err := core.NewProviderControl(path, app)
	if err != nil {
		return err
	}
	engine.SetProviderControl(control)
	return nil
}

func providerControlLocation(cfg *config.ProviderControlConfig) (string, string, error) {
	app := strings.TrimSpace(cfg.App)
	if app == "" {
		app = strings.TrimSpace(cfg.AppType)
	}
	if app == "" {
		return "", "", fmt.Errorf("provider_control.app is required")
	}
	if cfg.App != "" && cfg.AppType != "" && cfg.App != cfg.AppType {
		return "", "", fmt.Errorf("provider_control.app and app_type disagree")
	}
	path := strings.TrimSpace(cfg.ConfigFile)
	if path == "" && cfg.DBPath != "" {
		path = filepath.Join(filepath.Dir(cfg.DBPath), "control-api.json")
	}
	if path == "" || strings.HasPrefix(path, "~/") || strings.HasPrefix(path, "~\\") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", "", fmt.Errorf("resolve provider control home: %w", err)
		}
		if path == "" {
			path = filepath.Join(home, ".cc-switch", "control-api.json")
		} else {
			path = filepath.Join(home, path[2:])
		}
	}
	return path, strings.ToLower(app), nil
}
