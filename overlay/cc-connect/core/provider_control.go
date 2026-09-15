package core

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// ControlledProvider deliberately contains no credentials or upstream URLs.
type ControlledProvider struct {
	ID         string   `json:"id"`
	Name       string   `json:"name"`
	Model      string   `json:"model"`
	Models     []string `json:"models"`
	Selectable bool     `json:"selectable"`
}

type ProviderCatalog struct {
	CurrentID    string               `json:"current_id"`
	Providers    []ControlledProvider `json:"providers"`
	ProxyRunning bool                 `json:"proxy_running"`
	AutoFailover bool                 `json:"auto_failover"`
}

// ProviderController operates on the desktop's shared proxy, not agent sessions.
type ProviderController interface {
	Catalog(context.Context) (*ProviderCatalog, error)
	Select(context.Context, string) (*ProviderCatalog, error)
}

type fileProviderControl struct {
	configFile string
	app        string
	client     *http.Client
}

type providerControlCredentials struct {
	Enabled     bool     `json:"enabled"`
	Port        int      `json:"port"`
	Token       string   `json:"token"`
	AllowedApps []string `json:"allowed_apps"`
}

// NewProviderControl reads endpoint/credentials on every request, so rotating a
// token or repairing a port does not require restarting the messaging bridge.
// A numeric loopback address, no proxy, and no redirects keep its token local.
func NewProviderControl(configFile, app string) (ProviderController, error) {
	if strings.TrimSpace(configFile) == "" || app == "" {
		return nil, errors.New("provider control requires a config file and application")
	}
	for _, r := range app {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-') {
			return nil, errors.New("invalid provider control application")
		}
	}
	return &fileProviderControl{
		configFile: configFile,
		app:        app,
		client: &http.Client{
			Timeout:       3 * time.Second,
			Transport:     &http.Transport{Proxy: nil},
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}, nil
}

func (c *fileProviderControl) Catalog(ctx context.Context) (*ProviderCatalog, error) {
	return c.request(ctx, "", nil)
}

func (c *fileProviderControl) Select(ctx context.Context, id string) (*ProviderCatalog, error) {
	if id == "" || len(id) > 256 {
		return nil, errors.New("invalid provider ID")
	}
	return c.request(ctx, "/select", map[string]string{"id": id})
}

func (c *fileProviderControl) request(ctx context.Context, suffix string, payload any) (*ProviderCatalog, error) {
	raw, err := os.ReadFile(c.configFile)
	if err != nil {
		return nil, errors.New("cannot read provider control configuration")
	}
	var cfg providerControlCredentials
	if len(raw) > 65536 || json.Unmarshal(raw, &cfg) != nil {
		return nil, errors.New("invalid provider control configuration")
	}
	if !cfg.Enabled {
		return nil, errors.New("provider control API is disabled")
	}
	_, tokenErr := hex.DecodeString(cfg.Token)
	if cfg.Port < 1 || cfg.Port > 65535 || len(cfg.Token) < 32 || len(cfg.Token) > 256 || tokenErr != nil {
		return nil, errors.New("invalid provider control port or token")
	}
	allowed := false
	for _, app := range cfg.AllowedApps {
		allowed = allowed || app == c.app
	}
	if !allowed {
		return nil, errors.New("application is not allowed by provider control configuration")
	}
	method := http.MethodGet
	var body io.Reader
	if payload != nil {
		data, err := json.Marshal(payload)
		if err != nil {
			return nil, errors.New("invalid provider selection")
		}
		body = bytes.NewReader(data)
		method = http.MethodPost
	}
	endpoint := fmt.Sprintf("http://127.0.0.1:%d/api/v1/providers/%s%s", cfg.Port, c.app, suffix)
	req, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return nil, errors.New("cannot create provider control request")
	}
	req.Header.Set("Authorization", "Bearer "+cfg.Token)
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("cannot reach local provider control API: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// Never reflect arbitrary response bodies: proxy errors can contain keys.
		return nil, fmt.Errorf("provider control API returned HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, (1<<20)+1))
	if err != nil || len(data) > 1<<20 {
		return nil, errors.New("cannot read provider control response")
	}
	var catalog ProviderCatalog
	if json.Unmarshal(data, &catalog) != nil || catalog.Providers == nil {
		return nil, errors.New("invalid provider control response")
	}
	return &catalog, nil
}

// SetProviderControl is called during engine construction, before Start.
func (e *Engine) SetProviderControl(control ProviderController) { e.providerControl = control }

func (e *Engine) cmdCCS(p Platform, msg *Message, args []string) {
	if e.providerControl == nil {
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgCCSNotConfigured))
		return
	}
	if len(args) > 0 && strings.EqualFold(args[0], "pick") {
		e.cmdCCSPicker(p, msg, args[1:])
		return
	}
	target := ""
	if len(args) > 0 {
		switch strings.ToLower(args[0]) {
		case "list", "current":
			if len(args) != 1 {
				e.reply(p, msg.ReplyCtx, e.i18n.T(MsgCCSUsage))
				return
			}
		case "switch":
			target = strings.TrimSpace(strings.Join(args[1:], " "))
			if target == "" {
				e.reply(p, msg.ReplyCtx, e.i18n.T(MsgCCSUsage))
				return
			}
		default:
			e.reply(p, msg.ReplyCtx, e.i18n.T(MsgCCSUsage))
			return
		}
	}
	if target != "" {
		// Serialize direct commands with confirmed card selections as well.
		if !e.ccsApplyMu.TryLock() {
			e.reply(p, msg.ReplyCtx, e.i18n.T(MsgCCSBusy))
			return
		}
		defer e.ccsApplyMu.Unlock()
	}
	ctx, cancel := context.WithTimeout(e.ctx, 6*time.Second)
	defer cancel()
	catalog, err := e.providerControl.Catalog(ctx)
	if err != nil {
		e.reply(p, msg.ReplyCtx, e.i18n.Tf(MsgCCSUnavailable, err))
		return
	}
	notice := ""
	if target != "" {
		if !catalog.ProxyRunning || catalog.AutoFailover {
			e.reply(p, msg.ReplyCtx, e.i18n.T(MsgCCSBlocked))
			return
		}
		provider, ok := findControlledProvider(catalog, target)
		if !ok || !provider.Selectable {
			e.reply(p, msg.ReplyCtx, e.i18n.T(MsgCCSNotFound))
			return
		}
		catalog, err = e.providerControl.Select(ctx, provider.ID)
		if err != nil {
			e.reply(p, msg.ReplyCtx, e.i18n.Tf(MsgCCSUnavailable, err))
			return
		}
		if catalog.CurrentID != provider.ID {
			e.reply(p, msg.ReplyCtx, e.i18n.T(MsgCCSUnconfirmed))
			return
		}
		notice = e.i18n.Tf(MsgCCSSelected, provider.Name)
	}
	e.replyCCSMenu(p, msg, catalog, notice)
}

func findControlledProvider(catalog *ProviderCatalog, target string) (ControlledProvider, bool) {
	// Stable IDs take priority over display names; duplicate names fail closed.
	for _, provider := range catalog.Providers {
		if provider.ID == target {
			return provider, true
		}
	}
	var found ControlledProvider
	matches := 0
	for _, provider := range catalog.Providers {
		if strings.EqualFold(provider.Name, target) {
			found = provider
			matches++
		}
	}
	return found, matches == 1
}

func (e *Engine) renderProviderControlCard(catalog *ProviderCatalog, notice string) *Card {
	current := catalog.CurrentID
	if current == "" {
		current = e.i18n.T(MsgCCSNone)
	}
	for _, provider := range catalog.Providers {
		if provider.ID == catalog.CurrentID {
			current = provider.Name
			break
		}
	}
	card := NewCard().Title(e.i18n.Tf(MsgCCSTitle, e.name), "blue").
		Markdown(notice).Markdown(e.i18n.Tf(MsgCCSCurrent, current))
	canSelect := catalog.ProxyRunning && !catalog.AutoFailover
	if !canSelect {
		card.Markdown(e.i18n.T(MsgCCSBlocked))
	}
	if len(catalog.Providers) == 0 {
		card.Markdown(e.i18n.T(MsgProviderListEmpty))
	}
	for _, provider := range catalog.Providers {
		text := provider.Name
		if provider.Model != "" {
			text += " · " + provider.Model
		}
		if provider.ID == catalog.CurrentID {
			text = "▶ " + text
		}
		if canSelect && provider.Selectable && provider.ID != catalog.CurrentID {
			card.ListItemBtn(text, e.i18n.T(MsgCCSSelect), "default", fmt.Sprintf("cmd:/ccs switch %q", provider.ID))
		} else {
			card.Markdown(text)
		}
	}
	return card.Buttons(DefaultBtn(e.i18n.T(MsgCCSRefresh), "cmd:/ccs")).
		Note(e.i18n.T(MsgCCSShared)).Note(e.i18n.T(MsgCCSUsage)).Build()
}
