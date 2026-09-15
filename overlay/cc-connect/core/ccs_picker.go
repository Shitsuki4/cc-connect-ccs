package core

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	ccsPickerTTL      = 10 * time.Minute
	ccsPickerLimit    = 128
	ccsPickerPageSize = 50
)

// Picker state is short-lived and memory-only. Callback values contain an opaque
// nonce, never a model/provider name, credentials, or a persisted conversation ID.
// Each view is single-use and bound to its requesting user AND conversation.
// Selecting a dropdown only navigates; only the final Apply button can write.
type ccsPickerState struct {
	userID, sessionKey, scope string
	view, providerID, model   string
	modelBefore               string
	fingerprint               [32]byte
	catalog                   *ProviderCatalog
	page                      int
	created                   time.Time
}

func (e *Engine) ccsModelPickingAllowed(msg *Message) bool {
	e.userRolesMu.RLock()
	disabled, roles := e.disabledCmds, e.userRoles
	e.userRolesMu.RUnlock()
	if roles != nil {
		if role := roles.ResolveRole(msg.UserID); role != nil {
			disabled = role.DisabledCmds
		}
	}
	return !disabled["model"]
}

func (e *Engine) replyCCSMenu(p Platform, msg *Message, catalog *ProviderCatalog, notice string) {
	agent, _ := e.sessionContextForKey(msg.SessionKey)
	switcher, ok := agent.(ModelSwitcher)
	// Retain provider-only commands and text fallback for other agents/platforms.
	if !ok || !supportsCards(p) || !e.ccsModelPickingAllowed(msg) {
		e.replyWithCard(p, msg.ReplyCtx, e.renderProviderControlCard(catalog, notice))
		return
	}
	e.clearCCSPickers(msg)
	state := ccsPickerState{
		userID: msg.UserID, sessionKey: msg.SessionKey,
		scope: e.interactiveKeyForSessionKey(msg.SessionKey), view: "providers",
		modelBefore: switcher.GetModel(), catalog: cloneCCSCatalog(catalog),
		fingerprint: ccsCatalogFingerprint(catalog),
	}
	e.replyWithCard(p, msg.ReplyCtx, e.renderCCSPicker(state, notice))
}

func cloneCCSCatalog(catalog *ProviderCatalog) *ProviderCatalog {
	out := *catalog
	out.Providers = append([]ControlledProvider(nil), catalog.Providers...)
	for i := range out.Providers {
		out.Providers[i].Models = append([]string(nil), out.Providers[i].Models...)
	}
	return &out
}

func ccsCatalogFingerprint(catalog *ProviderCatalog) [32]byte {
	stable := cloneCCSCatalog(catalog)
	sort.Slice(stable.Providers, func(i, j int) bool { return stable.Providers[i].ID < stable.Providers[j].ID })
	for i := range stable.Providers {
		sort.Strings(stable.Providers[i].Models)
	}
	data, _ := json.Marshal(stable) // This public, concrete schema cannot fail to marshal.
	return sha256.Sum256(data)
}

func (e *Engine) clearCCSPickers(msg *Message) {
	e.ccsPickerMu.Lock()
	defer e.ccsPickerMu.Unlock()
	for token, state := range e.ccsPickers {
		if state.userID == msg.UserID && state.sessionKey == msg.SessionKey {
			delete(e.ccsPickers, token)
		}
	}
}

func (e *Engine) storeCCSPicker(state ccsPickerState) string {
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		slog.Error("ccs: cannot create picker nonce")
		return ""
	}
	state.created = time.Now()
	e.ccsPickerMu.Lock()
	defer e.ccsPickerMu.Unlock()
	if e.ccsPickers == nil {
		e.ccsPickers = make(map[string]*ccsPickerState)
	}
	oldest := ""
	for token, existing := range e.ccsPickers {
		if state.created.Sub(existing.created) >= ccsPickerTTL {
			delete(e.ccsPickers, token)
			continue
		}
		if oldest == "" || existing.created.Before(e.ccsPickers[oldest].created) {
			oldest = token
		}
	}
	if len(e.ccsPickers) >= ccsPickerLimit {
		delete(e.ccsPickers, oldest)
	}
	token := hex.EncodeToString(nonce[:])
	e.ccsPickers[token] = &state
	return token
}

func (e *Engine) takeCCSPicker(msg *Message, token string) (ccsPickerState, bool) {
	e.ccsPickerMu.Lock()
	defer e.ccsPickerMu.Unlock()
	state := e.ccsPickers[token]
	if state == nil || state.userID != msg.UserID || state.sessionKey != msg.SessionKey {
		return ccsPickerState{}, false
	}
	delete(e.ccsPickers, token)
	if time.Since(state.created) >= ccsPickerTTL {
		return ccsPickerState{}, false
	}
	return *state, true
}

func (e *Engine) ccsInfoCard(text string) *Card {
	return NewCard().InPlace().Title(e.i18n.Tf(MsgCCSPickerTitle, e.name), "blue").
		Markdown(text).Buttons(DefaultBtn(e.i18n.T(MsgCCSRefresh), "cmd:/ccs")).Build()
}

func controlledProviderModels(provider ControlledProvider) []string {
	var models []string
	seen := make(map[string]bool)
	for _, model := range append([]string{provider.Model}, provider.Models...) {
		model = strings.TrimSpace(model)
		if model != "" && len(model) <= 256 && !strings.ContainsAny(model, "\r\n\x00") && !seen[model] {
			seen[model] = true
			models = append(models, model)
		}
	}
	return models
}

func ccsShortLabel(text string) string {
	runes := []rune(text)
	if len(runes) > 80 {
		return string(runes[:79]) + "…"
	}
	return text
}

func ccsPickerAction(token, choice string) string { return "cmd:/ccs pick " + token + " " + choice }

func (e *Engine) renderCCSPicker(state ccsPickerState, notice string) *Card {
	token := e.storeCCSPicker(state)
	if token == "" {
		return e.ccsInfoCard(e.i18n.T(MsgCCSPickerExpired))
	}
	card := NewCard().InPlace().Title(e.i18n.Tf(MsgCCSPickerTitle, e.name), "blue").Markdown(notice)
	current, ok := findControlledProvider(state.catalog, state.catalog.CurrentID)
	currentName := e.i18n.T(MsgCCSNone)
	if ok {
		currentName = current.Name
	}
	model := state.modelBefore
	if model == "" {
		model = e.i18n.T(MsgCCSKeepModel)
	}
	card.Markdown(e.i18n.Tf(MsgCCSCurrent, currentName)).
		Markdown(e.i18n.Tf(MsgCCSProjectModel, model)).Divider()
	count := 0
	switch state.view {
	case "providers":
		count = e.renderCCSProviderOptions(card, state, token)
	case "models":
		count = e.renderCCSModelOptions(card, state, token)
	case "confirm":
		provider, found := findControlledProvider(state.catalog, state.providerID)
		if !found {
			return e.ccsInfoCard(e.i18n.T(MsgCCSPickerExpired))
		}
		target := state.model
		if target == "" {
			target = e.i18n.T(MsgCCSKeepModel)
		}
		card.Markdown(e.i18n.Tf(MsgCCSConfirmChoice, provider.Name, target))
		if !provider.Selectable {
			card.Markdown(e.i18n.T(MsgCCSNotSelectable))
		} else if !state.catalog.ProxyRunning || state.catalog.AutoFailover {
			card.Markdown(e.i18n.T(MsgCCSBlocked))
		} else {
			card.Buttons(PrimaryBtn(e.i18n.T(MsgCCSApply), ccsPickerAction(token, "apply")))
		}
	}
	if count > ccsPickerPageSize {
		var pages []CardButton
		if state.page > 0 {
			pages = append(pages, DefaultBtn(e.i18n.T(MsgCardPrev), ccsPickerAction(token, "prev")))
		}
		if (state.page+1)*ccsPickerPageSize < count {
			pages = append(pages, DefaultBtn(e.i18n.T(MsgCardNext), ccsPickerAction(token, "next")))
		}
		card.Buttons(pages...).Note(e.i18n.Tf(MsgCCSPage, state.page+1, (count+ccsPickerPageSize-1)/ccsPickerPageSize))
	}
	var nav []CardButton
	if state.view != "providers" {
		nav = append(nav, DefaultBtn(e.i18n.T(MsgCardBack), ccsPickerAction(token, "back")))
	}
	nav = append(nav, DefaultBtn(e.i18n.T(MsgCCSCancel), ccsPickerAction(token, "cancel")),
		DefaultBtn(e.i18n.T(MsgCCSRefresh), ccsPickerAction(token, "refresh")))
	return card.Buttons(nav...).Note(e.i18n.T(MsgCCSPickerScope)).Build()
}

func (e *Engine) renderCCSProviderOptions(card *CardBuilder, state ccsPickerState, token string) int {
	card.Markdown(e.i18n.T(MsgCCSChooseProvider))
	if !state.catalog.ProxyRunning || state.catalog.AutoFailover {
		card.Markdown(e.i18n.T(MsgCCSBlocked))
	}
	providers := state.catalog.Providers
	names := make(map[string]int)
	for _, provider := range providers {
		names[strings.ToLower(provider.Name)]++
	}
	var options []CardSelectOption
	for i := state.page * ccsPickerPageSize; i < len(providers) && i < (state.page+1)*ccsPickerPageSize; i++ {
		provider := providers[i]
		label := provider.Name
		if names[strings.ToLower(label)] > 1 {
			// Keep duplicates distinguishable even when a long name is truncated.
			label = fmt.Sprintf("%d · %s · %s", i+1, label, provider.ID)
		}
		if provider.ID == state.catalog.CurrentID {
			label = "▶ " + label
		}
		options = append(options, CardSelectOption{Text: ccsShortLabel(label), Value: ccsPickerAction(token, fmt.Sprintf("provider:%d", i))})
	}
	if len(options) == 0 {
		card.Markdown(e.i18n.T(MsgProviderListEmpty))
	}
	// No initial option: selecting the current provider must also open its models.
	card.Select(e.i18n.T(MsgCCSProviderPlaceholder), options, "")
	return len(providers)
}

func (e *Engine) renderCCSModelOptions(card *CardBuilder, state ccsPickerState, token string) int {
	provider, ok := findControlledProvider(state.catalog, state.providerID)
	if !ok {
		card.Markdown(e.i18n.T(MsgCCSPickerExpired))
		return 0
	}
	card.Markdown(e.i18n.Tf(MsgCCSChooseModel, provider.Name))
	models := controlledProviderModels(provider)
	var options []CardSelectOption
	for i := state.page * ccsPickerPageSize; i < len(models) && i < (state.page+1)*ccsPickerPageSize; i++ {
		label := models[i]
		if provider.ID == state.catalog.CurrentID && models[i] == state.modelBefore {
			label = "▶ " + label
		}
		options = append(options, CardSelectOption{Text: ccsShortLabel(label), Value: ccsPickerAction(token, fmt.Sprintf("model:%d", i))})
	}
	if len(models) == 0 {
		card.Markdown(e.i18n.T(MsgCCSNoModels)).
			Buttons(DefaultBtn(e.i18n.T(MsgCCSProviderOnly), ccsPickerAction(token, "provider-only")))
	}
	card.Select(e.i18n.T(MsgModelSelectPlaceholder), options, "")
	return len(models)
}

func (e *Engine) cmdCCSPicker(p Platform, msg *Message, args []string) {
	if len(args) != 2 {
		e.replyWithCard(p, msg.ReplyCtx, e.ccsInfoCard(e.i18n.T(MsgCCSPickerExpired)))
		return
	}
	state, ok := e.takeCCSPicker(msg, args[0])
	if !ok {
		e.replyWithCard(p, msg.ReplyCtx, e.ccsInfoCard(e.i18n.T(MsgCCSPickerExpired)))
		return
	}
	if args[1] == "cancel" {
		e.clearCCSPickers(msg)
		e.replyWithCard(p, msg.ReplyCtx, e.ccsInfoCard(e.i18n.T(MsgCCSCancelled)))
		return
	}
	ctx, cancel := context.WithTimeout(e.ctx, 6*time.Second)
	defer cancel()
	if state.view == "confirm" && args[1] == "apply" {
		e.applyCCSPicker(ctx, p, msg, state)
		return
	}
	catalog, err := e.providerControl.Catalog(ctx)
	if err != nil {
		e.replyWithCard(p, msg.ReplyCtx, e.ccsInfoCard(e.i18n.Tf(MsgCCSUnavailable, err)))
		return
	}
	if args[1] == "refresh" {
		e.replyCCSMenu(p, msg, catalog, "")
		return
	}
	agent, _ := e.sessionContextForKey(msg.SessionKey)
	switcher, supported := agent.(ModelSwitcher)
	if !supported || state.scope != e.interactiveKeyForSessionKey(msg.SessionKey) ||
		state.modelBefore != switcher.GetModel() || state.fingerprint != ccsCatalogFingerprint(catalog) {
		e.replyWithCard(p, msg.ReplyCtx, e.ccsInfoCard(e.i18n.T(MsgCCSPickerExpired)))
		return
	}
	if !advanceCCSPicker(&state, args[1]) {
		e.replyWithCard(p, msg.ReplyCtx, e.ccsInfoCard(e.i18n.T(MsgCCSPickerExpired)))
		return
	}
	e.replyWithCard(p, msg.ReplyCtx, e.renderCCSPicker(state, ""))
}

func advanceCCSPicker(state *ccsPickerState, action string) bool {
	provider, _ := findControlledProvider(state.catalog, state.providerID)
	models := controlledProviderModels(provider)
	count := len(state.catalog.Providers)
	if state.view == "models" {
		count = len(models)
	}
	switch action {
	case "back":
		if state.view == "confirm" {
			state.view, state.model, state.page = "models", "", 0
			return true
		}
		if state.view == "models" {
			state.view, state.providerID, state.page = "providers", "", 0
			return true
		}
	case "next", "prev":
		if state.view != "providers" && state.view != "models" {
			return false
		}
		next := state.page + 1
		if action == "prev" {
			next = state.page - 1
		}
		if next >= 0 && next*ccsPickerPageSize < count {
			state.page = next
			return true
		}
	case "provider-only":
		if state.view == "models" && len(models) == 0 {
			state.view, state.model = "confirm", ""
			return true
		}
	default:
		kind, raw, found := strings.Cut(action, ":")
		index, err := strconv.Atoi(raw)
		if !found || err != nil || index < state.page*ccsPickerPageSize || index >= (state.page+1)*ccsPickerPageSize {
			return false
		}
		if kind == "provider" && state.view == "providers" && index < len(state.catalog.Providers) {
			state.providerID = state.catalog.Providers[index].ID
			state.view, state.page = "models", 0
			return true
		}
		if kind == "model" && state.view == "models" && index < len(models) {
			state.model, state.view = models[index], "confirm"
			return true
		}
	}
	return false
}

func (e *Engine) applyCCSPicker(ctx context.Context, p Platform, msg *Message, state ccsPickerState) {
	replyError := func(key MsgKey) { e.replyWithCard(p, msg.ReplyCtx, e.ccsInfoCard(e.i18n.T(key))) }
	if !e.ccsApplyMu.TryLock() {
		replyError(MsgCCSBusy)
		return
	}
	defer e.ccsApplyMu.Unlock()
	// Read inside the apply lock, so another local selection cannot invalidate
	// our preflight check while we wait. External desktop changes are still
	// authoritative; the selection response must confirm the requested ID.
	catalog, err := e.providerControl.Catalog(ctx)
	if err != nil {
		e.replyWithCard(p, msg.ReplyCtx, e.ccsInfoCard(e.i18n.Tf(MsgCCSUnavailable, err)))
		return
	}
	if state.fingerprint != ccsCatalogFingerprint(catalog) {
		replyError(MsgCCSPickerExpired)
		return
	}
	provider, ok := findControlledProvider(catalog, state.providerID)
	if !ok || !provider.Selectable {
		replyError(MsgCCSNotSelectable)
		return
	}
	if !catalog.ProxyRunning || catalog.AutoFailover {
		replyError(MsgCCSBlocked)
		return
	}
	if state.model != "" && !slices.Contains(controlledProviderModels(provider), state.model) {
		replyError(MsgCCSPickerExpired)
		return
	}
	agent, sessions, interactiveKey, err := e.commandContext(p, msg)
	if err != nil || interactiveKey != state.scope {
		replyError(MsgCCSPickerExpired)
		return
	}
	switcher, ok := agent.(ModelSwitcher)
	if !ok || switcher.GetModel() != state.modelBefore {
		replyError(MsgCCSPickerExpired)
		return
	}
	// Do not interrupt a live turn. Hold the same lock normal messages use so
	// a new turn cannot start between changing the proxy and changing the model.
	session := sessions.GetOrCreateActive(msg.SessionKey)
	if !session.TryLock() {
		replyError(MsgCCSBusy)
		return
	}
	defer session.UnlockWithoutUpdate()
	if switcher.GetModel() != state.modelBefore || e.interactiveKeyForSessionKey(msg.SessionKey) != state.scope {
		replyError(MsgCCSPickerExpired)
		return
	}
	providerChanged := catalog.CurrentID != provider.ID
	if providerChanged {
		catalog, err = e.providerControl.Select(ctx, provider.ID)
		if err != nil || catalog == nil || catalog.CurrentID != provider.ID {
			replyError(MsgCCSUnconfirmed)
			return
		}
	}
	if err := e.applyCCSProjectModel(agent, sessions, interactiveKey, msg.SessionKey, state.model); err != nil {
		// No cross-process transaction: report partial success, never blindly
		// roll back a shared proxy that somebody else may now be using.
		slog.Warn("ccs: project model update failed", "provider_changed", providerChanged)
		key := MsgCCSModelApplyFailed
		if providerChanged {
			key = MsgCCSPartial
		}
		replyError(key)
		return
	}
	model := switcher.GetModel()
	if model == "" {
		model = e.i18n.T(MsgCCSKeepModel)
	}
	e.replyCCSMenu(p, msg, catalog, e.i18n.Tf(MsgCCSPairSelected, provider.Name, model))
}

// applyCCSProjectModel follows /model persistence and native-resume semantics.
func (e *Engine) applyCCSProjectModel(agent Agent, sessions *SessionManager, interactiveKey, sessionKey, target string) error {
	if target == "" || agent.(ModelSwitcher).GetModel() == target {
		return nil
	}
	if _, err := e.switchModelOnAgent(agent, target, agent == e.agent); err != nil {
		return err
	}
	e.persistWorkspaceModelOverride(interactiveKey, sessionKey, agent, target)
	e.cleanupInteractiveState(interactiveKey)
	// Never clear history or the native session ID when changing models.
	sessions.Save()
	return nil
}
