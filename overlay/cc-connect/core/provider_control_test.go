package core

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func TestCCSUnconfiguredCommandDoesNotReachAgent(t *testing.T) {
	e := newTestEngine()
	defer e.cancel()
	p := &stubPlatformEngine{n: "test"}
	msg := &Message{SessionKey: "test:user1", UserID: "user1", Platform: "test", ReplyCtx: "ctx"}
	if !e.handleCommand(p, msg, "/ccs") {
		t.Fatal("/ccs fell through to the AI agent instead of being handled as a command")
	}
	if len(p.getSent()) == 0 {
		t.Fatal("/ccs must explain that provider control is not configured")
	}
}

func writeControlConfig(t *testing.T, path, address, token string, apps []string) {
	t.Helper()
	u, err := url.Parse(address)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(providerControlCredentials{Enabled: true, Port: port, Token: token, AllowedApps: apps})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
}

func controlForServer(t *testing.T, server *httptest.Server) (ProviderController, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "control-api.json")
	writeControlConfig(t, path, server.URL, strings.Repeat("a", 64), []string{"test"})
	control, err := NewProviderControl(path, "test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(control.(*fileProviderControl).client.CloseIdleConnections)
	return control, path
}

// A real HTTP boundary for command and CUJ tests; no desktop database is used.
func newCCSFixture(t *testing.T) (ProviderController, *atomic.Int32) {
	t.Helper()
	var mu sync.Mutex
	current := "a"
	posts := &atomic.Int32{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+strings.Repeat("a", 64) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.URL.Path == "/api/v1/providers/test" && r.Method == http.MethodGet:
		case r.URL.Path == "/api/v1/providers/test/select" && r.Method == http.MethodPost:
			var request struct {
				ID string `json:"id"`
			}
			if r.Header.Get("Content-Type") != "application/json" || json.NewDecoder(r.Body).Decode(&request) != nil {
				http.Error(w, "invalid request", 400)
				return
			}
			if request.ID != "a" && request.ID != "b" {
				http.Error(w, "not found", 404)
				return
			}
			current = request.ID
			posts.Add(1)
		default:
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(ProviderCatalog{
			CurrentID: current, ProxyRunning: true,
			Providers: []ControlledProvider{
				{ID: "a", Name: "Alpha", Model: "model-a", Selectable: true},
				{ID: "b", Name: "Beta", Model: "model-b", Selectable: true},
			},
		})
	}))
	t.Cleanup(server.Close)
	control, _ := controlForServer(t, server)
	return control, posts
}

func TestCCSCatalogAndSwitchUseDesktopHTTP(t *testing.T) {
	control, posts := newCCSFixture(t)
	e := newTestEngine()
	defer e.cancel()
	e.SetProviderControl(control)
	p := &stubCardPlatform{stubPlatformEngine: stubPlatformEngine{n: "test"}}
	msg := &Message{SessionKey: "test:user1", UserID: "user1", Platform: "test", ReplyCtx: "ctx"}
	for _, command := range []string{"/ccs", "/CCS list", "/cc-switch current"} {
		if !e.handleCommand(p, msg, command) {
			t.Fatalf("unhandled %s", command)
		}
	}
	if posts.Load() != 0 {
		t.Fatal("viewing a menu must never switch a provider")
	}
	if len(p.repliedCards) != 3 {
		t.Fatalf("got %d cards", len(p.repliedCards))
	}
	card := p.repliedCards[0]
	if !strings.Contains(card.RenderText(), "Desktop selection: Alpha") {
		t.Fatal(card.RenderText())
	}
	callback := ""
	for _, element := range card.Elements {
		if item, ok := element.(CardListItem); ok {
			callback = item.BtnValue
		}
	}
	if callback != `cmd:/ccs switch "b"` {
		t.Fatalf("unsafe or missing command callback: %q", callback)
	}
	if !e.handleCommand(p, msg, strings.TrimPrefix(callback, "cmd:")) {
		t.Fatal("button command not handled")
	}
	if posts.Load() != 1 || !strings.Contains(p.repliedCards[3].RenderText(), "Desktop selection: Beta") {
		t.Fatal("selection was not reflected in the returned menu")
	}
}

func TestCCSInheritsDisabledProviderPolicy(t *testing.T) {
	for _, disabled := range []string{"provider", "ccs", "cc-switch"} {
		t.Run(disabled, func(t *testing.T) {
			control, posts := newCCSFixture(t)
			e := newTestEngine()
			defer e.cancel()
			e.SetProviderControl(control)
			e.SetDisabledCommands([]string{disabled})
			for _, command := range e.GetAllCommands() {
				if command.Command == "ccs" {
					t.Fatal("disabled command was advertised in the bot menu")
				}
			}
			p := &stubPlatformEngine{n: "test"}
			msg := &Message{SessionKey: "test:user1", UserID: "user1", ReplyCtx: "ctx"}
			if !e.handleCommand(p, msg, "/ccs switch Beta") {
				t.Fatal("command escaped policy gate")
			}
			if posts.Load() != 0 || !strings.Contains(strings.Join(p.getSent(), "\n"), "disabled") {
				t.Fatal(p.getSent())
			}
		})
	}
}

func TestCCSBlockedAndUnknownSelectionsDoNotWrite(t *testing.T) {
	for _, scenario := range []string{"proxy-stopped", "auto-failover", "not-selectable", "missing", "ambiguous"} {
		t.Run(scenario, func(t *testing.T) {
			var writes atomic.Int32
			catalog := ProviderCatalog{ProxyRunning: true, Providers: []ControlledProvider{{ID: "a", Name: "Alpha", Selectable: true}}}
			target := "Alpha"
			switch scenario {
			case "proxy-stopped":
				catalog.ProxyRunning = false
			case "auto-failover":
				catalog.AutoFailover = true
			case "not-selectable":
				catalog.Providers[0].Selectable = false
			case "missing":
				target = "missing"
			case "ambiguous":
				catalog.Providers = append(catalog.Providers, ControlledProvider{ID: "b", Name: "ALPHA", Selectable: true})
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet {
					writes.Add(1)
				}
				_ = json.NewEncoder(w).Encode(catalog)
			}))
			defer server.Close()
			control, _ := controlForServer(t, server)
			e := newTestEngine()
			defer e.cancel()
			e.SetProviderControl(control)
			p := &stubPlatformEngine{n: "test"}
			e.handleCommand(p, &Message{SessionKey: "test:user1"}, "/ccs switch "+target)
			if writes.Load() != 0 || len(p.getSent()) != 1 {
				t.Fatal("rejected selection made a write or failed to explain why")
			}
		})
	}
}

func TestProviderControlRejectsErrorsWithoutLeakingBodies(t *testing.T) {
	for _, status := range []int{301, 401, 403, 404, 409, 500} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(status)
				_, _ = fmt.Fprint(w, "SECRET-RESPONSE-BODY")
			}))
			defer server.Close()
			control, _ := controlForServer(t, server)
			_, err := control.Catalog(context.Background())
			if err == nil || !strings.Contains(err.Error(), strconv.Itoa(status)) || strings.Contains(err.Error(), "SECRET") {
				t.Fatalf("unsafe error: %v", err)
			}
		})
	}
}

func TestProviderControlNeverFollowsRedirects(t *testing.T) {
	var leaked atomic.Int32
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { leaked.Add(1) }))
	defer destination.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, destination.URL, http.StatusTemporaryRedirect)
	}))
	defer server.Close()
	control, _ := controlForServer(t, server)
	if _, err := control.Catalog(context.Background()); err == nil {
		t.Fatal("redirect accepted")
	}
	if leaked.Load() != 0 {
		t.Fatal("authorization could have escaped to a redirect target")
	}
}

func TestProviderControlReloadsCredentialsAndFailsClosed(t *testing.T) {
	var expected atomic.Value
	expected.Store(strings.Repeat("a", 64))
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Authorization") != "Bearer "+expected.Load().(string) {
			http.Error(w, "unauthorized", 401)
			return
		}
		_, _ = fmt.Fprint(w, `{"current_id":"desktop-selected","providers":[],"proxy_running":true,"api_key":"SECRET"}`)
	}))
	defer server.Close()
	control, path := controlForServer(t, server)
	for _, token := range []string{strings.Repeat("a", 64), strings.Repeat("b", 64)} {
		expected.Store(token)
		writeControlConfig(t, path, server.URL, token, []string{"test"})
		catalog, err := control.Catalog(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		data, _ := json.Marshal(catalog)
		if strings.Contains(string(data), "SECRET") || catalog.CurrentID != "desktop-selected" {
			t.Fatal("unsafe catalog")
		}
	}
	writeControlConfig(t, path, server.URL, strings.Repeat("b", 64), []string{"other"})
	if _, err := control.Catalog(context.Background()); err == nil {
		t.Fatal("application allowlist ignored")
	}
	writeControlConfig(t, path, server.URL, "bad-token", []string{"test"})
	if _, err := control.Catalog(context.Background()); err == nil {
		t.Fatal("invalid token accepted")
	}
	if calls.Load() != 2 {
		t.Fatal("invalid configurations made network requests")
	}
	if _, err := NewProviderControl(path, "../test"); err == nil {
		t.Fatal("path injection accepted")
	}
}

func TestProviderControlRejectsOversizeInvalidAndCancelledResponses(t *testing.T) {
	for _, body := range []string{"not JSON", "null", "{}", `{"providers":null}`, `{"providers":{}}`, strings.Repeat("x", (1<<20)+1)} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = fmt.Fprint(w, body) }))
		control, _ := controlForServer(t, server)
		if _, err := control.Catalog(context.Background()); err == nil {
			t.Fatal("invalid response accepted")
		}
		server.Close()
	}
	control, _ := newCCSFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := control.Catalog(ctx); err == nil {
		t.Fatal("request ignored context cancellation")
	}
}

func TestCCSSelectionMustBeConfirmedByDesktop(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(ProviderCatalog{
			CurrentID: "a", ProxyRunning: true,
			Providers: []ControlledProvider{{ID: "b", Name: "Beta", Selectable: true}},
		})
	}))
	defer server.Close()
	control, _ := controlForServer(t, server)
	e := newTestEngine()
	defer e.cancel()
	e.SetProviderControl(control)
	p := &stubPlatformEngine{n: "test"}
	e.handleCommand(p, &Message{SessionKey: "test:user1"}, "/ccs switch Beta")
	if got := strings.Join(p.getSent(), "\n"); !strings.Contains(got, "did not confirm") || strings.Contains(got, "Selected Beta") {
		t.Fatalf("unconfirmed selection was reported as successful: %s", got)
	}
}

func TestCCSStableIDWinsOverDisplayName(t *testing.T) {
	catalog := &ProviderCatalog{Providers: []ControlledProvider{
		{ID: "other", Name: "b"}, {ID: "b", Name: "Beta"},
	}}
	provider, found := findControlledProvider(catalog, "b")
	if !found || provider.ID != "b" {
		t.Fatal("display name shadowed a stable provider ID")
	}
}
