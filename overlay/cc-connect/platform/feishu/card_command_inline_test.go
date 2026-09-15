package feishu

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/core"
	lark "github.com/larksuite/oapi-sdk-go/v3"
	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher/callback"
)

// Only external boundaries are fake: the Feishu HTTP API, provider controller,
// and agent process. The engine, session manager, renderer, JSON callback, and
// normal command authorization/dispatch are real. No production configuration
// or credentials are loaded and no agent process is started.
type inlinePickerAgent struct {
	mu             sync.Mutex
	model          string
	starts, writes atomic.Int32
}

func (a *inlinePickerAgent) Name() string { return "fixture" }
func (a *inlinePickerAgent) StartSession(context.Context, string) (core.AgentSession, error) {
	a.starts.Add(1)
	return nil, errors.New("agent execution is not allowed in this fixture")
}
func (a *inlinePickerAgent) ListSessions(context.Context) ([]core.AgentSessionInfo, error) {
	return nil, nil
}
func (a *inlinePickerAgent) Stop() error { return nil }
func (a *inlinePickerAgent) SetModel(model string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.writes.Add(1)
	a.model = model
}
func (a *inlinePickerAgent) GetModel() string                                   { a.mu.Lock(); defer a.mu.Unlock(); return a.model }
func (a *inlinePickerAgent) AvailableModels(context.Context) []core.ModelOption { return nil }

type inlinePickerControl struct{ writes atomic.Int32 }

func (c *inlinePickerControl) Catalog(context.Context) (*core.ProviderCatalog, error) {
	return &core.ProviderCatalog{CurrentID: "a", ProxyRunning: true, Providers: []core.ControlledProvider{
		{ID: "a", Name: "Alpha", Model: "model-a", Models: []string{"model-a", "model-a-plus"}, Selectable: true},
		{ID: "b", Name: "Beta", Model: "model-b", Models: []string{"model-b", "model-b-plus"}, Selectable: true},
	}}, nil
}
func (c *inlinePickerControl) Select(context.Context, string) (*core.ProviderCatalog, error) {
	c.writes.Add(1)
	return nil, errors.New("provider selection is not allowed in this fixture")
}

type inlinePickerHTTP struct {
	method, path string
	card         map[string]any
}
type inlinePickerHarness struct {
	p        *interactivePlatform
	e        *core.Engine
	agent    *inlinePickerAgent
	control  *inlinePickerControl
	requests chan inlinePickerHTTP
	handled  chan struct{}
}

const inlinePickerKey = "feishu:oc_fixture_picker:ou_fixture_picker"

func newInlinePickerHarness(t *testing.T) *inlinePickerHarness {
	t.Helper()
	h := &inlinePickerHarness{requests: make(chan inlinePickerHTTP, 32), handled: make(chan struct{}, 32),
		agent: &inlinePickerAgent{model: "model-a"}, control: &inlinePickerControl{}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/open-apis/auth/v3/tenant_access_token/internal" {
			writeJSON(t, w, map[string]any{"code": 0, "expire": 7200, "tenant_access_token": "fixture-only-token"})
			return
		}
		if (r.Method == http.MethodPost && r.URL.Path == "/open-apis/im/v1/messages/om_command/reply") ||
			(r.Method == http.MethodPatch && r.URL.Path == "/open-apis/im/v1/messages/om_picker") {
			var body struct {
				Content string `json:"content"`
			}
			var card map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			if err := json.Unmarshal([]byte(body.Content), &card); err != nil {
				t.Error(err)
			}
			h.requests <- inlinePickerHTTP{r.Method, r.URL.Path, card}
			writeJSON(t, w, map[string]any{"code": 0, "data": map[string]any{"message_id": "om_picker"}})
			return
		}
		t.Errorf("unexpected fixture API request: %s %s", r.Method, r.URL.Path)
		http.Error(w, "unexpected fixture request", 400)
	}))
	t.Cleanup(server.Close)
	platform, err := New(map[string]any{"app_id": "cli_inline_picker", "app_secret": "fixture-only", "enable_feishu_card": true})
	if err != nil {
		t.Fatal(err)
	}
	h.p = platform.(*interactivePlatform)
	h.p.domain = server.URL
	h.p.client = lark.NewClient(h.p.appID, h.p.appSecret, lark.WithOpenBaseUrl(server.URL), lark.WithHttpClient(server.Client()), lark.WithEnableTokenCache(false))
	h.p.replayClient = lark.NewClient(h.p.appID, h.p.appSecret, lark.WithOpenBaseUrl(server.URL), lark.WithHttpClient(server.Client()), lark.WithEnableTokenCache(false))
	h.p.userNameCache.Store("ou_fixture_picker", "Fixture user")
	h.p.chatNameCache.Store("oc_fixture_picker", "Fixture chat")
	// A different card in this session must never be used for a delayed update.
	h.p.cardActionMsgIDs = map[string]string{inlinePickerKey: "om_unrelated"}
	h.e = core.NewEngine("picker-fixture", h.agent, []core.Platform{h.p}, filepath.Join(t.TempDir(), "sessions.json"), core.LangEnglish)
	h.e.SetProviderControl(h.control)
	h.e.SetModelSaveFunc(func(string) error { t.Error("preview persisted a model"); return nil })
	t.Cleanup(func() { _ = h.e.Stop() })
	h.p.handler = func(p core.Platform, msg *core.Message) {
		if msg.UserID != "ou_fixture_picker" || msg.SessionKey != inlinePickerKey {
			t.Error("callback lost its actor/session binding")
		}
		h.e.ReceiveMessage(p, msg)
		h.handled <- struct{}{}
	}
	return h
}
func (h *inlinePickerHarness) initial(t *testing.T) map[string]any {
	t.Helper()
	h.e.ReceiveMessage(h.p, &core.Message{Content: "/ccs", Platform: "feishu", UserID: "ou_fixture_picker", SessionKey: inlinePickerKey,
		ReplyCtx: replyContext{messageID: "om_command", chatID: "oc_fixture_picker", sessionKey: inlinePickerKey}})
	select {
	case r := <-h.requests:
		return r.card
	case <-time.After(3 * time.Second):
		t.Fatal("initial card not delivered")
		return nil
	}
}

// Build the same JSON shape that Feishu sends, from the exact rendered control
// metadata; do not invent update_card/session_key fields as the old tests did.
func inlinePickerEvent(t *testing.T, card map[string]any, choice string) *callback.CardActionTriggerEvent {
	t.Helper()
	var action map[string]any
	var walk func(any)
	matches := func(value string) bool { return value == choice || strings.HasSuffix(value, " "+choice) }
	walk = func(node any) {
		if action != nil {
			return
		}
		switch v := node.(type) {
		case []any:
			for _, child := range v {
				walk(child)
			}
		case map[string]any:
			if v["tag"] == "select_static" {
				options, _ := v["options"].([]any)
				for _, o := range options {
					option, _ := o.(map[string]any)
					value, _ := option["value"].(string)
					if matches(value) {
						action = map[string]any{"tag": "select_static", "option": value, "value": v["value"]}
						return
					}
				}
			}
			if v["tag"] == "button" {
				value, _ := v["value"].(map[string]any)
				command, _ := value["action"].(string)
				if matches(command) {
					action = map[string]any{"tag": "button", "value": value}
					return
				}
			}
			for _, child := range v {
				walk(child)
			}
		}
	}
	walk(card)
	if action == nil {
		t.Fatalf("rendered card has no %s control", choice)
	}
	raw, err := json.Marshal(map[string]any{"event": map[string]any{
		"operator": map[string]any{"open_id": "ou_fixture_picker"}, "action": action,
		"context": map[string]any{"open_chat_id": "oc_fixture_picker", "open_message_id": "om_picker"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	var event callback.CardActionTriggerEvent
	if err = json.Unmarshal(raw, &event); err != nil {
		t.Fatal(err)
	}
	return &event
}
func inlinePickerResponseMap(t *testing.T, response *callback.CardActionTriggerResponse) map[string]any {
	t.Helper()
	if response == nil || response.Card == nil || response.Card.Type != "raw" {
		t.Fatal("selection did not return the next card in the Feishu callback; the client can retain its old interactive view")
	}
	raw, err := json.Marshal(response.Card.Data)
	if err != nil {
		t.Fatal(err)
	}
	var card map[string]any
	if err = json.Unmarshal(raw, &card); err != nil {
		t.Fatal(err)
	}
	return card
}
func (h *inlinePickerHarness) click(t *testing.T, card map[string]any, choice string) map[string]any {
	t.Helper()
	response, err := h.p.onCardAction(inlinePickerEvent(t, card, choice))
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-h.handled:
	case <-time.After(3 * time.Second):
		t.Fatal("normal engine dispatch did not finish")
	}
	next := inlinePickerResponseMap(t, response)
	select {
	case r := <-h.requests:
		t.Fatalf("fast callback also sent an HTTP card update: %s", r.method)
	default:
	}
	return next
}

func TestRenderCardMap_InPlaceCommandsEnableSharedUpdates(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		card := core.NewCard().Title("Picker", "blue").Buttons(core.DefaultBtn("Refresh", "cmd:/ccs")).Build()
		card.UpdateOnAction = enabled
		got := decodeRenderedCard(t, card)["config"].(map[string]any)["update_multi"]
		if enabled && got != true {
			t.Fatal("in-place command cards must enable shared updates for delayed PATCH delivery")
		}
		if !enabled && got != nil {
			t.Fatal("ordinary cards unexpectedly changed their update mode")
		}
	}
}

func TestCUJ_FeishuCCSPicker_SelectProviderUpdatesOriginalCard(t *testing.T) {
	h := newInlinePickerHarness(t)
	providers := h.initial(t)
	models := h.click(t, providers, "provider:0")
	inlinePickerEvent(t, models, "model:1") // selected supplier's models are visible
	inlinePickerEvent(t, models, "back")    // bottom navigation changed from the provider screen
	confirm := h.click(t, models, "model:1")
	inlinePickerEvent(t, confirm, "apply") // only preview the button; do not apply
	models = h.click(t, confirm, "back")
	cancelled := h.click(t, models, "cancel")
	refreshed := h.click(t, cancelled, "cmd:/ccs")
	inlinePickerEvent(t, refreshed, "provider:1")
	if h.control.writes.Load() != 0 || h.agent.writes.Load() != 0 || h.agent.starts.Load() != 0 {
		t.Fatal("read-only interaction switched state or started an agent")
	}
}

func TestCardAction_InPlaceSlowReplyAcknowledgesThenPatchesOnlySource(t *testing.T) {
	h := newInlinePickerHarness(t)
	providers := h.initial(t)
	release := make(chan struct{})
	original := h.p.handler
	h.p.handler = func(p core.Platform, msg *core.Message) { <-release; original(p, msg) }
	start := time.Now()
	response, err := h.p.onCardAction(inlinePickerEvent(t, providers, "provider:0"))
	elapsed := time.Since(start)
	close(release)
	select {
	case <-h.handled:
	case <-time.After(4 * time.Second):
		t.Fatal("late card was lost or blocked")
	}
	if err != nil {
		t.Fatal(err)
	}
	if response == nil || response.Toast == nil || response.Card != nil {
		t.Fatal("slow callback must acknowledge before its deadline, then update asynchronously")
	}
	if elapsed >= 3*time.Second {
		t.Fatalf("callback exceeded Feishu's deadline: %v", elapsed)
	}
	select {
	case r := <-h.requests:
		if r.method != http.MethodPatch || r.path != "/open-apis/im/v1/messages/om_picker" {
			t.Fatal("late delivery did not PATCH the exact source")
		}
		inlinePickerEvent(t, r.card, "model:0")
		if r.card["config"].(map[string]any)["update_multi"] != true {
			t.Fatal("late update is not shared")
		}
	case <-time.After(time.Second):
		t.Fatal("late card was not delivered")
	}
	select {
	case <-h.requests:
		t.Fatal("late card was sent more than once")
	default:
	}
	if h.control.writes.Load() != 0 || h.agent.writes.Load() != 0 {
		t.Fatal("slow preview changed selection")
	}
}
