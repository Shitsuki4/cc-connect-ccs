package feishu

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/core"
	lark "github.com/larksuite/oapi-sdk-go/v3"
	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher/callback"
)

func TestRenderCardMap_InPlaceCommandControls(t *testing.T) {
	for _, inPlace := range []bool{false, true} {
		for _, sessionKey := range []string{"", "test:picker-user"} {
			t.Run(fmt.Sprintf("in-place=%t/key=%s", inPlace, sessionKey), func(t *testing.T) {
				card := core.NewCard().Title("Picker", "blue").
					Select("Provider", []core.CardSelectOption{{Text: "Alpha", Value: "cmd:/ccs pick token provider:0"}}, "").
					Select("Legacy navigation", []core.CardSelectOption{{Text: "Help", Value: "nav:/help"}}, "").
					ButtonsEqual(core.PrimaryBtn("Apply", "cmd:/ccs pick token apply"), core.DefaultBtn("Help", "nav:/help")).
					Buttons(core.DefaultBtn("Cancel", "cmd:/ccs pick token cancel")).
					ListItemBtn("An item", "Refresh", "default", "cmd:/ccs").Build()
				card.UpdateOnAction = inPlace
				var result any
				if err := json.Unmarshal([]byte(renderCard(card, sessionKey)), &result); err != nil {
					t.Fatal(err)
				}
				controls := 0
				var walk func(any)
				walk = func(node any) {
					switch v := node.(type) {
					case []any:
						for _, child := range v {
							walk(child)
						}
					case map[string]any:
						if v["tag"] == "button" || v["tag"] == "select_static" {
							controls++
							value, _ := v["value"].(map[string]any)
							action, _ := value["action"].(string)
							if options, ok := v["options"].([]any); ok {
								action, _ = options[0].(map[string]any)["value"].(string)
							}
							wantUpdate := inPlace && strings.HasPrefix(action, "cmd:")
							if (value["update_card"] == "true") != wantUpdate {
								t.Errorf("wrong in-place marker for %s: %#v", action, value)
							}
							if sessionKey != "" && value["session_key"] != sessionKey {
								t.Error("lost session binding")
							}
							if sessionKey == "" && value["session_key"] != nil {
								t.Error("invented a session key")
							}
						}
						for _, child := range v {
							walk(child)
						}
					}
				}
				walk(result)
				if controls != 6 {
					t.Fatalf("got %d controls, want 6", controls)
				}
			})
		}
	}
}

func TestCardAction_InPlaceCommandsUseNormalMessageDispatch(t *testing.T) {
	for _, dropdown := range []bool{false, true} {
		for _, update := range []bool{false, true} {
			t.Run(fmt.Sprintf("dropdown=%t/update=%t", dropdown, update), func(t *testing.T) {
				platform, err := New(map[string]any{"app_id": "cli_fixture", "app_secret": "fixture-only", "enable_feishu_card": true})
				if err != nil {
					t.Fatal(err)
				}
				p := platform.(*interactivePlatform)
				messages := make(chan *core.Message, 2)
				p.handler = func(_ core.Platform, msg *core.Message) { messages <- msg }
				var navCalls atomic.Int32
				p.cardNavHandler = func(string, string) *core.Card { navCalls.Add(1); return nil }
				command := "cmd:/ccs pick fixture-token apply"
				action := &callback.CallBackAction{Value: map[string]any{"session_key": "test:picker-user"}}
				if update {
					action.Value["update_card"] = "true"
				}
				if dropdown {
					action.Option = command
				} else {
					action.Value["action"] = command
				}
				response, err := p.onCardAction(&callback.CardActionTriggerEvent{Event: &callback.CardActionTriggerRequest{
					Operator: &callback.Operator{OpenID: "ou_picker"}, Action: action,
					Context: &callback.Context{OpenChatID: "oc_picker", OpenMessageID: "om_exact_source"},
				}})
				if err != nil {
					t.Fatal(err)
				}
				if response != nil && response.Card != nil {
					t.Fatal("command bypassed the engine by returning an immediate card")
				}
				select {
				case msg := <-messages:
					if msg.Content != strings.TrimPrefix(command, "cmd:") || msg.UserID != "ou_picker" || msg.SessionKey != "test:picker-user" {
						t.Fatalf("wrong dispatched message: %#v", msg)
					}
					rc, ok := msg.ReplyCtx.(replyContext)
					if !ok || rc.messageID != "om_exact_source" || rc.updateCard != update {
						t.Fatalf("wrong source context: %#v", msg.ReplyCtx)
					}
				case <-time.After(2 * time.Second):
					t.Fatal("command was not dispatched")
				}
				if navCalls.Load() != 0 {
					t.Fatal("command bypassed normal authorization through card navigation")
				}
				select {
				case <-messages:
					t.Fatal("command dispatched twice")
				default:
				}
			})
		}
	}
}

func TestReplyCard_InPlaceTargetsSourceAndFallsBackWithoutReexecuting(t *testing.T) {
	for _, mode := range []string{"success", "patch-error", "token-refresh"} {
		t.Run(mode, func(t *testing.T) {
			var authCalls, patchCalls, replyCalls, commandCalls atomic.Int32
			card := core.NewCard().InPlace().Title("Model picker", "blue").
				Select("Model", []core.CardSelectOption{{Text: "Model A", Value: "cmd:/ccs pick fixture model:0"}}, "").Build()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch {
				case r.URL.Path == "/open-apis/auth/v3/tenant_access_token/internal":
					n := authCalls.Add(1)
					token := "fixture-fresh-token"
					if mode == "token-refresh" && n == 1 {
						token = "fixture-stale-token"
					}
					writeJSON(t, w, map[string]any{"code": 0, "expire": 7200, "tenant_access_token": token})
				case r.Method == http.MethodPatch && r.URL.Path == "/open-apis/im/v1/messages/om_exact_source":
					patchCalls.Add(1)
					var body struct {
						Content string `json:"content"`
					}
					if json.NewDecoder(r.Body).Decode(&body) != nil || !strings.Contains(body.Content, "select_static") || !strings.Contains(body.Content, "update_card") {
						t.Error("PATCH did not contain the next interactive card")
					}
					if mode == "patch-error" {
						writeJSON(t, w, map[string]any{"code": 230001, "msg": "fixture message cannot be edited"})
					} else if mode == "token-refresh" && r.Header.Get("Authorization") == "Bearer fixture-stale-token" {
						writeJSON(t, w, map[string]any{"code": 99991663, "msg": "Invalid access token for authorization"})
					} else {
						writeJSON(t, w, map[string]any{"code": 0, "msg": "success"})
					}
				case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/om_exact_source/reply"):
					replyCalls.Add(1)
					writeJSON(t, w, map[string]any{"code": 0, "data": map[string]any{"message_id": "om_fallback"}})
				default:
					t.Errorf("unexpected request (possibly the wrong source card): %s %s", r.Method, r.URL.Path)
					http.Error(w, "unexpected request", 400)
				}
			}))
			defer server.Close()
			appID := "cli_picker_" + mode
			p := &interactivePlatform{Platform: &Platform{
				platformName: "feishu", domain: server.URL, appID: appID, appSecret: "fixture-only",
				client:       lark.NewClient(appID, "fixture-only", lark.WithOpenBaseUrl(server.URL), lark.WithHttpClient(server.Client())),
				replayClient: lark.NewClient(appID, "fixture-only", lark.WithEnableTokenCache(false), lark.WithOpenBaseUrl(server.URL), lark.WithHttpClient(server.Client())),
				// This is deliberately a different card in the same session.
				cardActionMsgIDs: map[string]string{"test:picker-user": "om_unrelated_newest_card"},
			}}
			p.handler = func(core.Platform, *core.Message) { commandCalls.Add(1) }
			err := p.ReplyCard(context.Background(), replyContext{
				messageID: "om_exact_source", chatID: "oc_picker", sessionKey: "test:picker-user", updateCard: true,
			}, card)
			if err != nil {
				t.Fatal(err)
			}
			if patchCalls.Load() < 1 || commandCalls.Load() != 0 {
				t.Fatal("delivery reexecuted a command or did not PATCH")
			}
			if mode == "patch-error" {
				if patchCalls.Load() != 1 || replyCalls.Load() != 1 {
					t.Fatalf("PATCH failure: patches=%d replies=%d", patchCalls.Load(), replyCalls.Load())
				}
			} else if replyCalls.Load() != 0 {
				t.Fatal("successful PATCH also sent a new message")
			}
			if mode == "token-refresh" && (patchCalls.Load() < 2 || authCalls.Load() < 2) {
				t.Fatal("PATCH did not refresh an invalid tenant token")
			}
		})
	}
}
