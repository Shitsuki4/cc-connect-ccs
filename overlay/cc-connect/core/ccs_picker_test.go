package core

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"
)

// These fixtures use only temporary files and a private HTTP server. They never
// load a desktop control file, contact the real proxy, or start an agent process.
type ccsPickerHTTP struct {
	mu          sync.Mutex
	catalog     *ProviderCatalog
	posts       atomic.Int32
	getStatus   int
	postStatus  int
	unconfirmed bool
}

func newCCSPickerHTTP(t *testing.T) (ProviderController, *ccsPickerHTTP) {
	t.Helper()
	f := &ccsPickerHTTP{catalog: &ProviderCatalog{
		CurrentID: "a", ProxyRunning: true,
		Providers: []ControlledProvider{
			{ID: "a", Name: "Alpha", Model: "model-a", Models: []string{"model-a", "model-a-plus"}, Selectable: true},
			{ID: "b", Name: "Beta", Model: "model-b", Models: []string{"model-b", "model-b-plus"}, Selectable: true},
		},
	}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+strings.Repeat("a", 64) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/providers/test":
			if f.getStatus != 0 {
				http.Error(w, "fixture unavailable", f.getStatus)
				return
			}
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/providers/test/select":
			f.posts.Add(1)
			if f.postStatus != 0 {
				http.Error(w, "fixture rejected selection", f.postStatus)
				return
			}
			var input struct {
				ID string `json:"id"`
			}
			if json.NewDecoder(r.Body).Decode(&input) != nil {
				http.Error(w, "bad input", http.StatusBadRequest)
				return
			}
			if _, ok := findControlledProvider(f.catalog, input.ID); !ok {
				http.NotFound(w, r)
				return
			}
			if !f.unconfirmed {
				f.catalog.CurrentID = input.ID
			}
		default:
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(f.catalog)
	}))
	t.Cleanup(server.Close)
	control, _ := controlForServer(t, server)
	return control, f
}

func (f *ccsPickerHTTP) change(fn func(*ProviderCatalog)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f.catalog)
}

type ccsPickerAgent struct {
	stubAgent
	mu    sync.Mutex
	model string
}

func (a *ccsPickerAgent) SetModel(model string) { a.mu.Lock(); defer a.mu.Unlock(); a.model = model }
func (a *ccsPickerAgent) GetModel() string      { a.mu.Lock(); defer a.mu.Unlock(); return a.model }
func (a *ccsPickerAgent) AvailableModels(context.Context) []ModelOption {
	// The picker must use the selected desktop provider's catalog, not this list.
	return []ModelOption{{Name: "unrelated-agent-model"}}
}

type ccsPickerEnv struct {
	t     *testing.T
	e     *Engine
	p     *stubCardPlatform
	a     *ccsPickerAgent
	f     *ccsPickerHTTP
	msg   *Message
	path  string
	saves atomic.Int32
}

func newCCSPickerEnv(t *testing.T) *ccsPickerEnv {
	t.Helper()
	a := &ccsPickerAgent{model: "model-a"}
	p := &stubCardPlatform{stubPlatformEngine: stubPlatformEngine{n: "test"}}
	path := filepath.Join(t.TempDir(), "sessions.json")
	e := NewEngine("test", a, []Platform{p}, path, LangEnglish)
	t.Cleanup(e.cancel)
	control, f := newCCSPickerHTTP(t)
	e.SetProviderControl(control)
	env := &ccsPickerEnv{t: t, e: e, p: p, a: a, f: f, path: path,
		msg: &Message{SessionKey: "test:picker-user", UserID: "picker-user", Platform: "test", ReplyCtx: "ctx"}}
	e.modelSaveFunc = func(string) error { env.saves.Add(1); return nil }
	return env
}

func lastCCSCard(t *testing.T, p *stubCardPlatform) *Card {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.repliedCards) == 0 {
		t.Fatal("no card reply")
	}
	return p.repliedCards[len(p.repliedCards)-1]
}

func ccsCardCommands(card *Card) []string {
	var out []string
	for _, element := range card.Elements {
		switch elem := element.(type) {
		case CardSelect:
			for _, option := range elem.Options {
				out = append(out, option.Value)
			}
		case CardActions:
			for _, button := range elem.Buttons {
				out = append(out, button.Value)
			}
		case CardListItem:
			out = append(out, elem.BtnValue)
		}
	}
	return out
}

func ccsCardCommand(t *testing.T, card *Card, action string) string {
	t.Helper()
	for _, value := range ccsCardCommands(card) {
		if strings.HasSuffix(value, " "+action) {
			return strings.TrimPrefix(value, "cmd:")
		}
	}
	t.Fatalf("missing %q in card: %s", action, card.RenderText())
	return ""
}

func ccsCardSelect(t *testing.T, card *Card) CardSelect {
	t.Helper()
	for _, element := range card.Elements {
		if selectElem, ok := element.(CardSelect); ok {
			return selectElem
		}
	}
	t.Fatal("missing dropdown: ", card.RenderText())
	return CardSelect{}
}

func (env *ccsPickerEnv) command(command string) *Card {
	env.t.Helper()
	if !env.e.handleCommand(env.p, env.msg, command) {
		env.t.Fatal("command was not handled: ", command)
	}
	return lastCCSCard(env.t, env.p)
}

func (env *ccsPickerEnv) choose(action string) *Card {
	env.t.Helper()
	return env.command(ccsCardCommand(env.t, lastCCSCard(env.t, env.p), action))
}

func (env *ccsPickerEnv) confirm(provider, model int) string {
	env.t.Helper()
	env.command("/ccs")
	env.choose(fmt.Sprintf("provider:%d", provider))
	env.choose(fmt.Sprintf("model:%d", model))
	return ccsCardCommand(env.t, lastCCSCard(env.t, env.p), "apply")
}

func (env *ccsPickerEnv) assertUnchanged() {
	env.t.Helper()
	if env.f.posts.Load() != 0 || env.saves.Load() != 0 || env.a.GetModel() != "model-a" {
		env.t.Fatalf("unexpected writes: posts=%d saves=%d model=%s", env.f.posts.Load(), env.saves.Load(), env.a.GetModel())
	}
}

func TestCCSPickerBrowseConfirmPreservesHistoryAndNativeResume(t *testing.T) {
	env := newCCSPickerEnv(t)
	session := env.e.sessions.GetOrCreateActive(env.msg.SessionKey)
	session.SetAgentSessionID("native-resume-fixture", "stub")
	session.AddHistory("user", "keep this fixture history")
	session.AddHistory("assistant", "remembered")
	history := session.GetHistory(0)
	env.e.sessions.Save()
	before, err := os.ReadFile(env.path)
	if err != nil {
		t.Fatal(err)
	}

	card := env.command("/ccs")
	if !card.UpdateOnAction || len(ccsCardSelect(t, card).Options) != 2 {
		t.Fatal("expected an in-place provider dropdown")
	}
	if ccsCardSelect(t, card).InitValue != "" {
		t.Fatal("current provider must remain selectable")
	}
	card = env.choose("provider:1")
	models := ccsCardSelect(t, card)
	if len(models.Options) != 2 || strings.Contains(card.RenderText(), "unrelated-agent-model") {
		t.Fatal("models did not come from the selected provider")
	}
	card = env.choose("model:1")
	if !strings.Contains(card.RenderText(), "model-b-plus") || !strings.Contains(card.RenderText(), "Beta") {
		t.Fatal("confirmation does not show the pending pair")
	}
	env.assertUnchanged()
	afterBrowse, err := os.ReadFile(env.path)
	if err != nil || string(before) != string(afterBrowse) {
		t.Fatal("browsing changed the session store")
	}

	card = env.choose("apply")
	if env.f.posts.Load() != 1 || env.saves.Load() != 1 || env.a.GetModel() != "model-b-plus" {
		t.Fatalf("wrong confirmed selection: posts=%d saves=%d model=%s", env.f.posts.Load(), env.saves.Load(), env.a.GetModel())
	}
	if !strings.Contains(card.RenderText(), "Applied · Provider: Beta · Project model: model-b-plus") {
		t.Fatal(card.RenderText())
	}
	if !card.UpdateOnAction {
		t.Fatal("workspace/card rendering dropped the in-place flag")
	}
	for _, store := range []*SessionManager{env.e.sessions, NewSessionManager(env.path)} {
		s := store.GetOrCreateActive(env.msg.SessionKey)
		// Compare serialized history: disk reload intentionally strips the
		// monotonic portion of time.Time, which reflect.DeepEqual would reject.
		wantHistory, _ := json.Marshal(history)
		gotHistory, _ := json.Marshal(s.GetHistory(0))
		if s.GetAgentSessionID() != "native-resume-fixture" || string(gotHistory) != string(wantHistory) {
			t.Fatal("selection lost history or native resume ID")
		}
	}
	if session.Busy() {
		t.Fatal("apply left the conversation locked")
	}
}

func TestCCSPickerSameProviderChangesOnlyProjectModel(t *testing.T) {
	env := newCCSPickerEnv(t)
	card := env.command(env.confirm(0, 1))
	if env.f.posts.Load() != 0 || env.saves.Load() != 1 || env.a.GetModel() != "model-a-plus" {
		t.Fatal("same-provider model selection unnecessarily changed the desktop")
	}
	if !strings.Contains(card.RenderText(), "Project model: model-a-plus") {
		t.Fatal(card.RenderText())
	}
}

func TestCCSPickerBackRefreshAndCancelAreReadOnly(t *testing.T) {
	env := newCCSPickerEnv(t)
	env.confirm(1, 1)
	card := env.choose("back")
	if !strings.Contains(card.RenderText(), "Choose a model for Beta") {
		t.Fatal(card.RenderText())
	}
	card = env.choose("back")
	if !strings.Contains(card.RenderText(), "Choose a provider") {
		t.Fatal(card.RenderText())
	}
	env.choose("refresh")
	card = env.choose("cancel")
	if !strings.Contains(card.RenderText(), "Cancelled") {
		t.Fatal(card.RenderText())
	}
	env.assertUnchanged()
}

func TestCCSPickerRejectsStaleConfirmations(t *testing.T) {
	for _, scenario := range []string{"expired", "reopened", "current-provider", "catalog-model", "project-model", "workspace"} {
		t.Run(scenario, func(t *testing.T) {
			env := newCCSPickerEnv(t)
			command := env.confirm(1, 1)
			switch scenario {
			case "expired":
				token := strings.Fields(command)[2]
				env.e.ccsPickerMu.Lock()
				env.e.ccsPickers[token].created = time.Now().Add(-ccsPickerTTL - time.Second)
				env.e.ccsPickerMu.Unlock()
			case "reopened":
				env.command("/cc-switch current")
			case "current-provider":
				env.f.change(func(c *ProviderCatalog) { c.CurrentID = "b" })
			case "catalog-model":
				env.f.change(func(c *ProviderCatalog) { c.Providers[1].Models = []string{"replacement"} })
			case "project-model":
				env.a.SetModel("changed-elsewhere")
			case "workspace":
				env.e.bindSendWorkDir(env.msg.SessionKey, t.TempDir())
			}
			card := env.command(command)
			if !strings.Contains(card.RenderText(), "expired") || env.f.posts.Load() != 0 || env.saves.Load() != 0 {
				t.Fatal("stale confirmation was not rejected: ", card.RenderText())
			}
		})
	}
}

func TestCCSPickerForeignCallbackCannotConsumeOwnerToken(t *testing.T) {
	for _, field := range []string{"user", "session"} {
		t.Run(field, func(t *testing.T) {
			env := newCCSPickerEnv(t)
			command := env.confirm(1, 1)
			other := *env.msg
			if field == "user" {
				other.UserID = "other-user"
			} else {
				other.SessionKey = "test:other-conversation"
			}
			env.e.handleCommand(env.p, &other, command)
			env.assertUnchanged()
			if !strings.Contains(lastCCSCard(t, env.p).RenderText(), "expired") {
				t.Fatal("foreign callback accepted")
			}
			env.command(command)
			if env.f.posts.Load() != 1 {
				t.Fatal("foreign callback consumed the owner's token")
			}
		})
	}
}

func TestCCSPickerRepeatedApplyWritesAtMostOnce(t *testing.T) {
	env := newCCSPickerEnv(t)
	command := env.confirm(1, 1)
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() { defer wg.Done(); env.e.handleCommand(env.p, env.msg, command) }()
	}
	wg.Wait()
	if env.f.posts.Load() != 1 || env.saves.Load() != 1 {
		t.Fatal("a repeated callback applied more than once")
	}
}

func TestCCSPickerRejectsBusyConversationOrSelection(t *testing.T) {
	for _, scenario := range []string{"conversation", "selection", "direct-command"} {
		t.Run(scenario, func(t *testing.T) {
			env := newCCSPickerEnv(t)
			command := env.confirm(1, 1)
			if scenario == "conversation" {
				session := env.e.sessions.GetOrCreateActive(env.msg.SessionKey)
				if !session.TryLock() {
					t.Fatal("fixture session is already busy")
				}
				defer session.UnlockWithoutUpdate()
			} else {
				env.e.ccsApplyMu.Lock()
				defer env.e.ccsApplyMu.Unlock()
			}
			if scenario == "direct-command" {
				env.e.handleCommand(env.p, env.msg, "/ccs switch Beta")
				if !strings.Contains(strings.Join(env.p.getSent(), "\n"), "still in progress") {
					t.Fatal(env.p.getSent())
				}
			} else if card := env.command(command); !strings.Contains(card.RenderText(), "still in progress") {
				t.Fatal(card.RenderText())
			}
			env.assertUnchanged()
		})
	}
}

func TestCCSPickerDisabledCommandsAndRolesGateCallbacks(t *testing.T) {
	for _, disabled := range []string{"provider", "model", "ccs", "cc-switch"} {
		for _, rolePolicy := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/role=%t", disabled, rolePolicy), func(t *testing.T) {
				env := newCCSPickerEnv(t)
				command := env.confirm(1, 1)
				if rolePolicy {
					roles := NewUserRoleManager()
					roles.Configure("", []RoleInput{{Name: "restricted", UserIDs: []string{env.msg.UserID}, DisabledCommands: []string{disabled}}})
					env.e.SetUserRoles(roles)
				} else {
					env.e.SetDisabledCommands([]string{disabled})
				}
				env.e.handleCommand(env.p, env.msg, command)
				if !strings.Contains(strings.Join(env.p.getSent(), "\n"), "disabled") {
					t.Fatal("callback bypassed command policy")
				}
				env.assertUnchanged()
			})
		}
	}
}

func TestCCSPickerProxyAndSelectableGuards(t *testing.T) {
	for _, scenario := range []string{"stopped", "failover", "unselectable"} {
		t.Run(scenario, func(t *testing.T) {
			env := newCCSPickerEnv(t)
			env.f.change(func(c *ProviderCatalog) {
				switch scenario {
				case "stopped":
					c.ProxyRunning = false
				case "failover":
					c.AutoFailover = true
				case "unselectable":
					c.Providers[1].Selectable = false
				}
			})
			env.command("/ccs")
			env.choose("provider:1")
			card := env.choose("model:1")
			for _, command := range ccsCardCommands(card) {
				if strings.HasSuffix(command, " apply") {
					t.Fatal("blocked selection advertises Apply")
				}
			}
			// A forged Apply must still be checked, not just hidden in the UI.
			command := strings.TrimSuffix(ccsCardCommand(t, card, "cancel"), "cancel") + "apply"
			card = env.command(command)
			want := env.e.i18n.T(MsgCCSBlocked)
			if scenario == "unselectable" {
				want = env.e.i18n.T(MsgCCSNotSelectable)
			}
			if !strings.Contains(card.RenderText(), want) {
				t.Fatal(card.RenderText())
			}
			env.assertUnchanged()
		})
	}
}

func TestCCSPickerProviderOnlyWhenCatalogHasNoModels(t *testing.T) {
	env := newCCSPickerEnv(t)
	env.f.change(func(c *ProviderCatalog) { c.Providers[1].Model = ""; c.Providers[1].Models = nil })
	env.command("/ccs")
	card := env.choose("provider:1")
	if !strings.Contains(card.RenderText(), "no configured models") {
		t.Fatal(card.RenderText())
	}
	for _, elem := range card.Elements {
		if _, ok := elem.(CardSelect); ok {
			t.Fatal("invented a model list")
		}
	}
	env.choose("provider-only")
	env.assertUnchanged()
	env.choose("apply")
	if env.f.posts.Load() != 1 || env.saves.Load() != 0 || env.a.GetModel() != "model-a" {
		t.Fatal("provider-only selection changed the model")
	}
}

func TestCCSPickerModelSaveFailureDoesNotHidePartialResult(t *testing.T) {
	for _, provider := range []int{0, 1} {
		t.Run(fmt.Sprintf("provider:%d", provider), func(t *testing.T) {
			env := newCCSPickerEnv(t)
			env.e.modelSaveFunc = func(string) error { return errors.New("fixture save failed") }
			card := env.command(env.confirm(provider, 1))
			want := "The project model could not be saved"
			if provider == 1 {
				want = "The provider changed, but the project model could not be saved"
			}
			if !strings.Contains(card.RenderText(), want) || strings.Contains(card.RenderText(), "Applied ·") {
				t.Fatal(card.RenderText())
			}
			if env.f.posts.Load() != int32(provider) || env.a.GetModel() != "model-a" {
				t.Fatal("unexpected rollback or failed model applied")
			}
		})
	}
}

func TestCCSPickerUnconfirmedProviderNeverChangesModel(t *testing.T) {
	for _, scenario := range []string{"error", "wrong-current"} {
		t.Run(scenario, func(t *testing.T) {
			env := newCCSPickerEnv(t)
			command := env.confirm(1, 1)
			env.f.mu.Lock()
			if scenario == "error" {
				env.f.postStatus = 500
			} else {
				env.f.unconfirmed = true
			}
			env.f.mu.Unlock()
			card := env.command(command)
			if !strings.Contains(card.RenderText(), "did not confirm") || env.saves.Load() != 0 || env.a.GetModel() != "model-a" {
				t.Fatal("model changed without confirmed provider: ", card.RenderText())
			}
		})
	}
}

func TestCCSPickerCatalogReorderingIsHarmless(t *testing.T) {
	env := newCCSPickerEnv(t)
	command := env.confirm(1, 1)
	env.f.change(func(c *ProviderCatalog) {
		c.Providers[0], c.Providers[1] = c.Providers[1], c.Providers[0]
		c.Providers[0].Models[0], c.Providers[0].Models[1] = c.Providers[0].Models[1], c.Providers[0].Models[0]
	})
	env.command(command)
	if env.f.posts.Load() != 1 || env.a.GetModel() != "model-b-plus" {
		t.Fatal("catalog ordering changed the indexed selection")
	}
}

func TestCCSPickerPaginationAndOpaqueCallbacks(t *testing.T) {
	env := newCCSPickerEnv(t)
	env.f.change(func(c *ProviderCatalog) {
		c.Providers = nil
		for i := range 51 {
			models := []string{}
			for j := range 51 {
				models = append(models, fmt.Sprintf("vendor/model-%d", j))
			}
			c.Providers = append(c.Providers, ControlledProvider{ID: fmt.Sprintf("provider-%d", i), Name: strings.Repeat("供应商", 35), Models: models, Selectable: true})
		}
		c.CurrentID = "provider-0"
	})
	card := env.command("/ccs")
	selectElem := ccsCardSelect(t, card)
	if len(selectElem.Options) != 50 || selectElem.Options[0].Text == selectElem.Options[1].Text {
		t.Fatal("invalid provider page or indistinguishable duplicate names")
	}
	check := func(card *Card) {
		t.Helper()
		for _, command := range ccsCardCommands(card) {
			if len(command) > 64 || strings.Contains(command, "vendor/") || strings.Contains(command, "供应商") {
				t.Fatalf("unsafe callback %q", command)
			}
		}
		for _, option := range ccsCardSelect(t, card).Options {
			if utf8.RuneCountInString(option.Text) > 80 {
				t.Fatal("option label exceeds limit")
			}
		}
	}
	check(card)
	card = env.choose("next")
	if len(ccsCardSelect(t, card).Options) != 1 {
		t.Fatal("wrong second provider page")
	}
	card = env.choose("provider:50")
	check(card)
	card = env.choose("next")
	if len(ccsCardSelect(t, card).Options) != 1 {
		t.Fatal("wrong second model page")
	}
	env.choose("prev")
	env.choose("next")
	card = env.choose("model:50")
	if !strings.Contains(card.RenderText(), "vendor/model-50") {
		t.Fatal("wrong paginated model")
	}
	env.assertUnchanged()
}

func TestCCSPickerTreatsSpecialModelNamesAsLiterals(t *testing.T) {
	env := newCCSPickerEnv(t)
	model := `vendor/a model "quoted"; /model switch not-a-command`
	env.f.change(func(c *ProviderCatalog) { c.Providers[1].Models = []string{model} })
	env.command(env.confirm(1, 1))
	if env.a.GetModel() != model || env.saves.Load() != 1 {
		t.Fatal("model label was parsed as a command instead of a literal")
	}
}

func TestCCSPickerTokenStoreIsBoundedExpiringAndSingleUse(t *testing.T) {
	e := newTestEngine()
	defer e.cancel()
	msg := &Message{UserID: "u", SessionKey: "test:u"}
	tokens := map[string]bool{}
	last := ""
	for range ccsPickerLimit + 3 {
		last = e.storeCCSPicker(ccsPickerState{userID: msg.UserID, sessionKey: msg.SessionKey})
		decoded, err := hex.DecodeString(last)
		if err != nil || len(decoded) != 16 || tokens[last] {
			t.Fatal("invalid or repeated nonce")
		}
		tokens[last] = true
	}
	if len(e.ccsPickers) != ccsPickerLimit {
		t.Fatal("unbounded picker cache")
	}
	if _, ok := e.takeCCSPicker(msg, last); !ok {
		t.Fatal("fresh token rejected")
	}
	if _, ok := e.takeCCSPicker(msg, last); ok {
		t.Fatal("token reused")
	}
	for _, state := range e.ccsPickers {
		state.created = time.Now().Add(-ccsPickerTTL)
	}
	e.storeCCSPicker(ccsPickerState{userID: msg.UserID, sessionKey: msg.SessionKey})
	if len(e.ccsPickers) != 1 {
		t.Fatal("expired states were not pruned")
	}
}

// CUJ boundary doubles: card text is recorded as the message a user sees, and
// the fake agent reports its actual runtime model on the next conversation turn.
type ccsCUJPlatform struct{ *stubCardPlatform }

func (p *ccsCUJPlatform) ReplyCard(ctx context.Context, rctx any, card *Card) error {
	if err := p.stubCardPlatform.ReplyCard(ctx, rctx, card); err != nil {
		return err
	}
	return p.stubPlatformEngine.Reply(ctx, rctx, card.RenderText())
}

func (p *ccsCUJPlatform) SendCard(ctx context.Context, rctx any, card *Card) error {
	if err := p.stubCardPlatform.SendCard(ctx, rctx, card); err != nil {
		return err
	}
	return p.stubPlatformEngine.Send(ctx, rctx, card.RenderText())
}

type ccsCUJAgent struct {
	*cujAgent
	model     ccsPickerAgent
	resumeMu  sync.Mutex
	resumeIDs []string
}

func (a *ccsCUJAgent) SetModel(model string) { a.model.SetModel(model) }
func (a *ccsCUJAgent) GetModel() string      { return a.model.GetModel() }
func (a *ccsCUJAgent) AvailableModels(ctx context.Context) []ModelOption {
	return a.model.AvailableModels(ctx)
}
func (a *ccsCUJAgent) StartSession(ctx context.Context, resume string) (AgentSession, error) {
	a.resumeMu.Lock()
	a.resumeIDs = append(a.resumeIDs, resume)
	a.resumeMu.Unlock()
	s, err := a.cujAgent.StartSession(ctx, resume)
	if err != nil {
		return nil, err
	}
	session := s.(*cujAgentSession)
	session.mu.Lock()
	session.reply = "model in use: " + a.GetModel()
	session.mu.Unlock()
	return session, nil
}
func (a *ccsCUJAgent) lastResumeID() string {
	a.resumeMu.Lock()
	defer a.resumeMu.Unlock()
	if len(a.resumeIDs) == 0 {
		return ""
	}
	return a.resumeIDs[len(a.resumeIDs)-1]
}

func TestCCSPickerProviderOnlyFallbacks(t *testing.T) {
	for _, scenario := range []string{"unsupported-agent", "plain-text-platform", "model-disabled"} {
		t.Run(scenario, func(t *testing.T) {
			env := newCCSPickerEnv(t)
			if scenario == "unsupported-agent" {
				env.e.agent = &stubAgent{}
			}
			if scenario == "model-disabled" {
				env.e.SetDisabledCommands([]string{"model"})
			}
			if scenario == "plain-text-platform" {
				p := &stubPlatformEngine{n: "test"}
				env.e.handleCommand(p, env.msg, "/ccs")
				if !strings.Contains(strings.Join(p.getSent(), "\n"), "Desktop selection: Alpha") {
					t.Fatal(p.getSent())
				}
			} else {
				card := env.command("/ccs")
				for _, element := range card.Elements {
					if _, ok := element.(CardSelect); ok {
						t.Fatal("model picker shown without model capability or permission")
					}
				}
				if !strings.Contains(card.RenderText(), "Desktop selection: Alpha") {
					t.Fatal(card.RenderText())
				}
			}
			env.assertUnchanged()
		})
	}
}

func TestCCSPickerEmptyCatalogAndNoOpSelection(t *testing.T) {
	t.Run("empty", func(t *testing.T) {
		env := newCCSPickerEnv(t)
		env.f.change(func(c *ProviderCatalog) { c.Providers = []ControlledProvider{}; c.CurrentID = "" })
		card := env.command("/ccs")
		for _, element := range card.Elements {
			if _, ok := element.(CardSelect); ok {
				t.Fatal("empty dropdown rendered")
			}
		}
		if !strings.Contains(card.RenderText(), env.e.i18n.T(MsgProviderListEmpty)) {
			t.Fatal(card.RenderText())
		}
		env.choose("cancel")
		env.assertUnchanged()
	})
	t.Run("no-op", func(t *testing.T) {
		env := newCCSPickerEnv(t)
		card := env.command(env.confirm(0, 0))
		if !strings.Contains(card.RenderText(), "Applied · Provider: Alpha · Project model: model-a") {
			t.Fatal(card.RenderText())
		}
		env.assertUnchanged()
	})
}

func TestCCSPickerCatalogFailureBlocksApplyButNotCancel(t *testing.T) {
	for _, action := range []string{"apply", "cancel"} {
		t.Run(action, func(t *testing.T) {
			env := newCCSPickerEnv(t)
			env.confirm(1, 1)
			env.f.mu.Lock()
			env.f.getStatus = 500
			env.f.mu.Unlock()
			card := env.choose(action)
			want := "unavailable"
			if action == "cancel" {
				want = "Cancelled"
			}
			if !strings.Contains(card.RenderText(), want) {
				t.Fatal(card.RenderText())
			}
			env.assertUnchanged()
		})
	}
}

func TestCCSPickerRejectsForgedNavigation(t *testing.T) {
	for _, action := range []string{"apply", "model:0", "provider:-1", "provider:2", "provider:999999999999999999999999", "next", "prev", "back", "provider-only", "nonsense", "cancel extra"} {
		t.Run(action, func(t *testing.T) {
			env := newCCSPickerEnv(t)
			card := env.command("/ccs")
			command := strings.TrimSuffix(ccsCardCommand(t, card, "cancel"), "cancel") + action
			card = env.command(command)
			if !strings.Contains(card.RenderText(), "expired") {
				t.Fatal("invalid navigation accepted: ", card.RenderText())
			}
			env.assertUnchanged()
		})
	}
}

func TestCCSPickerReusesActiveAgentProviderModelPersistence(t *testing.T) {
	env := newCCSPickerEnv(t)
	a := &stubModelModeAgent{model: "model-a", active: "proxy", providers: []ProviderConfig{{Name: "proxy", Model: "model-a"}}}
	env.e.agent = a
	savedName, savedModel := "", ""
	env.e.providerModelSaveFunc = func(name, model string) error { savedName, savedModel = name, model; return nil }
	env.command(env.confirm(1, 1))
	if savedName != "proxy" || savedModel != "model-b-plus" || a.GetModel() != "model-b-plus" || a.GetActiveProvider().Model != "model-b-plus" {
		t.Fatal("picker bypassed existing agent-provider model persistence")
	}
	if env.saves.Load() != 0 {
		t.Fatal("saved a second, unrelated global model")
	}
}

func TestCCSPickerTranslationsCoverAllSupportedLanguages(t *testing.T) {
	keys := []MsgKey{MsgCCSNotSelectable, MsgCCSPickerTitle, MsgCCSPickerExpired, MsgCCSKeepModel, MsgCCSProjectModel, MsgCCSConfirmChoice,
		MsgCCSApply, MsgCCSPage, MsgCCSCancel, MsgCCSPickerScope, MsgCCSChooseProvider, MsgCCSProviderPlaceholder,
		MsgCCSChooseModel, MsgCCSNoModels, MsgCCSProviderOnly, MsgCCSCancelled, MsgCCSBusy, MsgCCSPartial,
		MsgCCSModelApplyFailed, MsgCCSPairSelected}
	for _, key := range keys {
		for _, language := range []Language{LangEnglish, LangChinese, LangTraditionalChinese, LangJapanese, LangSpanish} {
			if messages[key][language] == "" {
				t.Errorf("missing %s translation for %s", language, key)
			}
		}
	}
}

func TestCCSPickerUsesBoundWorkspaceModelAndPreservesBothHistories(t *testing.T) {
	env := newCCSPickerEnv(t)
	env.e.SetMultiWorkspace(t.TempDir(), filepath.Join(t.TempDir(), "bindings.json"))
	statePath := filepath.Join(t.TempDir(), "project-state.json")
	env.e.SetProjectStateStore(NewProjectStateStore(statePath))
	workspace := normalizeWorkspacePath(t.TempDir())
	env.e.workspaceBindings.Bind("project:test", "C-ccs-model", "channel", workspace)
	env.msg.SessionKey = "feishu:C-ccs-model:picker-user"
	ws := env.e.workspacePool.GetOrCreate(workspace)
	wsAgent := &ccsPickerAgent{model: "model-a"}
	ws.agent = wsAgent
	ws.sessions = NewSessionManager(filepath.Join(t.TempDir(), "workspace-sessions.json"))
	globalSession := env.e.sessions.GetOrCreateActive(env.msg.SessionKey)
	globalSession.SetAgentSessionID("global-native-id", "stub")
	globalSession.AddHistory("user", "global history fixture")
	wsSession := ws.sessions.GetOrCreateActive(env.msg.SessionKey)
	wsSession.SetAgentSessionID("workspace-native-id", "stub")
	wsSession.AddHistory("user", "workspace history fixture")
	env.command(env.confirm(1, 1))
	if wsAgent.GetModel() != "model-b-plus" || env.a.GetModel() != "model-a" || env.saves.Load() != 0 {
		t.Fatal("workspace selection changed the global project model")
	}
	if got := NewProjectStateStore(statePath).WorkspaceModelOverride(workspace); got != "model-b-plus" {
		t.Fatalf("workspace override not persisted: %q", got)
	}
	if globalSession.GetAgentSessionID() != "global-native-id" || wsSession.GetAgentSessionID() != "workspace-native-id" ||
		len(globalSession.GetHistory(0)) != 1 || len(wsSession.GetHistory(0)) != 1 {
		t.Fatal("model picker reset global or workspace conversation history")
	}
}
