package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	claudemodels "github.com/router-for-me/CLIProxyAPI/v8/internal/client/claude/models"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/home"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/tidwall/gjson"
)

const (
	destinationClaudeModel = "claude-dest-test-5-5"
	// destinationAliasModel has no claude- prefix; it is Anthropic only by registry provider.
	destinationAliasModel = "dest-test-opus-alias"
	destinationGPTModel   = "gpt-dest-test-6-1"
	// destinationSuffixModel is registered only under its full suffixed ID.
	destinationSuffixModel = "dest-test-suffix-only(8192)"
	// destinationCompatAlias is a neutral alias of a third-party provider whose upstream model is Claude.
	destinationCompatAlias = "dest-test-compat-alias"
)

type destinationExecutor struct {
	provider string
	mu       sync.Mutex
	models   []string
}

func (e *destinationExecutor) Identifier() string { return e.provider }

func (e *destinationExecutor) record(model string) {
	e.mu.Lock()
	e.models = append(e.models, model)
	e.mu.Unlock()
}

func (e *destinationExecutor) calls() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.models)
}

func (e *destinationExecutor) Execute(_ context.Context, _ *auth.Auth, req coreexecutor.Request, _ coreexecutor.Options) (coreexecutor.Response, error) {
	e.record(req.Model)
	return coreexecutor.Response{Payload: []byte(`{"id":"msg_1","type":"message","role":"assistant","model":"` + req.Model + `","content":[{"type":"text","text":"ok"}],"object":"response","status":"completed","output":[],"choices":[]}`)}, nil
}

func (e *destinationExecutor) ExecuteStream(_ context.Context, _ *auth.Auth, req coreexecutor.Request, _ coreexecutor.Options) (*coreexecutor.StreamResult, error) {
	e.record(req.Model)
	chunks := make(chan coreexecutor.StreamChunk, 1)
	chunks <- coreexecutor.StreamChunk{Payload: []byte(`{"type":"response.completed","response":{"id":"resp-1","output":[]}}`)}
	close(chunks)
	return &coreexecutor.StreamResult{Chunks: chunks}, nil
}

func (e *destinationExecutor) Refresh(_ context.Context, a *auth.Auth) (*auth.Auth, error) {
	return a, nil
}

func (e *destinationExecutor) CountTokens(_ context.Context, _ *auth.Auth, req coreexecutor.Request, _ coreexecutor.Options) (coreexecutor.Response, error) {
	e.record(req.Model)
	return coreexecutor.Response{Payload: []byte(`{"input_tokens":1}`)}, nil
}

func (e *destinationExecutor) HttpRequest(context.Context, *auth.Auth, *http.Request) (*http.Response, error) {
	return nil, nil
}

func newDestinationTestServer(t *testing.T, restrict bool) (*Server, *destinationExecutor, *destinationExecutor) {
	t.Helper()
	server := newTestServer(t)
	setRestrictAnthropicModels(server, restrict)
	claudeExec := &destinationExecutor{provider: "claude"}
	codexExec := &destinationExecutor{provider: "codex"}
	server.handlers.AuthManager.RegisterExecutor(claudeExec)
	server.handlers.AuthManager.RegisterExecutor(codexExec)
	for _, entry := range []struct {
		id, provider string
		models       []string
	}{
		{id: t.Name() + "-claude", provider: "claude", models: []string{destinationClaudeModel, destinationAliasModel, destinationSuffixModel}},
		{id: t.Name() + "-codex", provider: "codex", models: []string{destinationGPTModel}},
	} {
		credential := &auth.Auth{ID: entry.id, Provider: entry.provider, Status: auth.StatusActive, Attributes: map[string]string{"websockets": "true"}}
		if _, errRegister := server.handlers.AuthManager.Register(context.Background(), credential); errRegister != nil {
			t.Fatal(errRegister)
		}
		infos := make([]*registry.ModelInfo, 0, len(entry.models))
		for _, model := range entry.models {
			infos = append(infos, &registry.ModelInfo{ID: model, Object: "model", OwnedBy: entry.provider + "-owner", Type: entry.provider})
		}
		registry.GetGlobalRegistry().RegisterClient(entry.id, entry.provider, infos)
		id := entry.id
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
	}
	compatID := t.Name() + "-compat"
	registry.GetGlobalRegistry().RegisterClient(compatID, "dest-test-compat", []*registry.ModelInfo{{ID: destinationCompatAlias, MetadataModelID: "anthropic/claude-dest-upstream", Object: "model", OwnedBy: "dest-test-compat"}})
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(compatID) })
	return server, claudeExec, codexExec
}

// setRestrictAnthropicModels applies the flag through the same hot-reload path the config watcher uses.
func setRestrictAnthropicModels(server *Server, restrict bool) {
	updated := *server.cfg
	updated.ClaudeCode.RestrictAnthropicModels = restrict
	server.UpdateClients(&updated)
}

func serveDestination(t *testing.T, server *Server, method, path, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	var request *http.Request
	if body == "" {
		request = httptest.NewRequest(method, path, nil)
	} else {
		request = httptest.NewRequest(method, path, strings.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
	}
	request.Header.Set("Authorization", "Bearer test-key")
	for key, value := range headers {
		request.Header.Set(key, value)
	}
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)
	return recorder
}

func listedModelIDs(body []byte) []string {
	var ids []string
	for _, key := range []string{"data", "models"} {
		for _, entry := range gjson.GetBytes(body, key).Array() {
			for _, field := range []string{"id", "slug", "name"} {
				if value := entry.Get(field).String(); value != "" {
					ids = append(ids, strings.TrimPrefix(value, "models/"))
					break
				}
			}
		}
	}
	return ids
}

func containsModel(ids []string, model string) bool {
	for _, id := range ids {
		if id == model {
			return true
		}
	}
	return false
}

func messagesBody(model string) string {
	return `{"model":"` + model + `","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`
}

func TestClaudeCodeDestinationServesClaudeAndGPTModels(t *testing.T) {
	// The restriction is enabled to prove the prefix stays unrestricted.
	server, claudeExec, codexExec := newDestinationTestServer(t, true)

	for _, model := range []string{destinationClaudeModel, destinationAliasModel, destinationGPTModel} {
		rr := serveDestination(t, server, http.MethodPost, "/claude-code/v1/messages?beta=true", messagesBody(model), map[string]string{"Anthropic-Version": "2023-06-01"})
		if rr.Code != http.StatusOK {
			t.Fatalf("/claude-code/v1/messages %s status = %d; body=%s", model, rr.Code, rr.Body.String())
		}
	}
	rr := serveDestination(t, server, http.MethodPost, "/claude-code/v1/messages/count_tokens", messagesBody(destinationClaudeModel), nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("count_tokens status = %d; body=%s", rr.Code, rr.Body.String())
	}
	if claudeExec.calls() != 3 || codexExec.calls() != 1 {
		t.Fatalf("claude/codex executor calls = %d/%d, want 3/1", claudeExec.calls(), codexExec.calls())
	}

	rr = serveDestination(t, server, http.MethodGet, "/claude-code/v1/models", "", nil)
	ids := listedModelIDs(rr.Body.Bytes())
	if rr.Code != http.StatusOK || !containsModel(ids, destinationClaudeModel) || !containsModel(ids, destinationGPTModel) {
		t.Fatalf("/claude-code/v1/models status=%d ids=%v", rr.Code, ids)
	}
	// The Anthropic-format list keeps its existing cloaking of non-Claude IDs; all five models must remain.
	rr = serveDestination(t, server, http.MethodGet, "/claude-code/v1/models", "", map[string]string{"Anthropic-Version": "2023-06-01"})
	ids = listedModelIDs(rr.Body.Bytes())
	if rr.Code != http.StatusOK || !containsModel(ids, destinationClaudeModel) || len(ids) != 5 {
		t.Fatalf("/claude-code/v1/models Anthropic format status=%d ids=%v", rr.Code, ids)
	}

	if rr = serveDestination(t, server, http.MethodGet, "/claude-code/healthz", "", nil); rr.Code != http.StatusNotFound {
		t.Fatalf("/claude-code/healthz status = %d, want 404 (only /v1 is mounted)", rr.Code)
	}
}

func TestDefaultDestinationModelListsOmitAnthropicModels(t *testing.T) {
	server, _, _ := newDestinationTestServer(t, false)

	for _, path := range []string{"/v1/models", "/v1/models?client_version=0.166.0", "/v1/models?client_version=cpa", "/v1beta/models"} {
		rr := serveDestination(t, server, http.MethodGet, path, "", nil)
		ids := listedModelIDs(rr.Body.Bytes())
		if rr.Code != http.StatusOK {
			t.Fatalf("%s status = %d; body=%s", path, rr.Code, rr.Body.String())
		}
		if containsModel(ids, destinationClaudeModel) || containsModel(ids, destinationAliasModel) || containsModel(ids, destinationSuffixModel) || containsModel(ids, destinationCompatAlias) {
			t.Fatalf("%s lists Anthropic models: %v", path, ids)
		}
		if path == "/v1/models" && !containsModel(ids, destinationGPTModel) {
			t.Fatalf("%s lost GPT models: %v", path, ids)
		}
	}
	if rr := serveDestination(t, server, http.MethodGet, "/v1/models/"+destinationClaudeModel, "", nil); rr.Code != http.StatusNotFound {
		t.Fatalf("model detail status = %d, want 404; body=%s", rr.Code, rr.Body.String())
	}

	// Anthropic-format lists stay complete for unmigrated Claude Code clients until the flag is enabled.
	anthropic := map[string]string{"Anthropic-Version": "2023-06-01"}
	if ids := listedModelIDs(serveDestination(t, server, http.MethodGet, "/v1/models", "", anthropic).Body.Bytes()); !containsModel(ids, destinationClaudeModel) {
		t.Fatalf("flag off: Anthropic-format list = %v, want Claude models", ids)
	}
	setRestrictAnthropicModels(server, true)
	ids := listedModelIDs(serveDestination(t, server, http.MethodGet, "/v1/models", "", anthropic).Body.Bytes())
	if containsModel(ids, destinationClaudeModel) || containsModel(ids, claudemodels.EnsureClaudeModelIDPrefix(destinationAliasModel)) {
		t.Fatalf("flag on: Anthropic-format list = %v, want no Anthropic models", ids)
	}
	// The cloaked GPT entry (claude-fable-5-dd-<reversed>) is not Anthropic and must stay listed.
	if !containsModel(ids, claudemodels.EnsureClaudeModelIDPrefix(destinationGPTModel)) {
		t.Fatalf("flag on: Anthropic-format list = %v, want cloaked GPT model", ids)
	}
}

func TestDefaultDestinationHomeCodexCatalogOmitsAnthropicModels(t *testing.T) {
	server := newTestServer(t)
	server.cfg.Home.Enabled = true
	engine := gin.New()
	engine.GET("/v1/models", server.unifiedModelsHandler(nil, nil))
	handler := wrapClaudeCodeDestinationHandler(engine)
	client := newHomeCatalogClient(t, `{"claude":[{"id":"claude-home-test-5-5"},{"id":"home-neutral-alias"}],"codex":[{"id":"gpt-home-test-6-1"}]}`)
	previousHome := home.Current()
	home.SetCurrent(client)
	t.Cleanup(func() { home.SetCurrent(previousHome) })

	for _, test := range []struct {
		path       string
		wantClaude bool
	}{
		{path: "/v1/models?client_version=0.166.0"},
		{path: "/v1/models"},
		{path: "/claude-code/v1/models?client_version=0.166.0", wantClaude: true},
	} {
		request := httptest.NewRequest(http.MethodGet, test.path, nil)
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		ids := listedModelIDs(recorder.Body.Bytes())
		if recorder.Code != http.StatusOK || !containsModel(ids, "gpt-home-test-6-1") {
			t.Fatalf("%s status = %d ids=%v body=%s", test.path, recorder.Code, ids, recorder.Body.String())
		}
		if got := containsModel(ids, "claude-home-test-5-5") || containsModel(ids, "home-neutral-alias"); got != test.wantClaude {
			t.Fatalf("%s lists Claude = %t, want %t: %v", test.path, got, test.wantClaude, ids)
		}
	}
}

func TestDefaultDestinationRejectsAnthropicModelsWhenRestricted(t *testing.T) {
	server, claudeExec, codexExec := newDestinationTestServer(t, true)

	for _, model := range []string{destinationClaudeModel, destinationAliasModel, destinationClaudeModel + "(high)", destinationSuffixModel, destinationCompatAlias, claudemodels.EnsureClaudeModelIDPrefix(destinationAliasModel)} {
		for _, test := range []struct{ path, body string }{
			{path: "/v1/messages", body: messagesBody(model)},
			{path: "/v1/messages/count_tokens", body: messagesBody(model)},
			{path: "/v1/chat/completions", body: `{"model":"` + model + `","messages":[{"role":"user","content":"hi"}]}`},
			{path: "/v1/completions", body: `{"model":"` + model + `","prompt":"hi"}`},
			{path: "/v1/responses", body: `{"model":"` + model + `","input":"hi"}`},
			{path: "/v1/responses", body: `{"model":"` + model + `","input":"hi","stream":true}`},
			{path: "/v1/responses/compact", body: `{"model":"` + model + `","input":"hi"}`},
			{path: "/backend-api/codex/responses", body: `{"model":"` + model + `","input":"hi","stream":true}`},
		} {
			rr := serveDestination(t, server, http.MethodPost, test.path, test.body, nil)
			if rr.Code != http.StatusForbidden || !strings.Contains(rr.Body.String(), handlers.AnthropicModelsClaudeCodeOnlyMessage) {
				t.Fatalf("%s %s status = %d, want 403 with message; body=%s", test.path, model, rr.Code, rr.Body.String())
			}
		}
	}
	if claudeExec.calls() != 0 {
		t.Fatalf("Claude executor called %d times on the default destination", claudeExec.calls())
	}

	rr := serveDestination(t, server, http.MethodPost, "/v1/chat/completions", `{"model":"`+destinationGPTModel+`","messages":[{"role":"user","content":"hi"}]}`, nil)
	if rr.Code != http.StatusOK || codexExec.calls() != 1 {
		t.Fatalf("GPT chat completions status = %d calls = %d; body=%s", rr.Code, codexExec.calls(), rr.Body.String())
	}

	httpServer := httptest.NewServer(server.Handler())
	defer httpServer.Close()
	wsURL := "ws" + strings.TrimPrefix(httpServer.URL, "http")
	for _, test := range []struct {
		path, model string
		wantStatus  int
	}{
		{path: "/v1/responses", model: destinationClaudeModel, wantStatus: http.StatusForbidden},
		{path: "/v1/responses", model: destinationGPTModel},
		{path: "/claude-code/v1/responses", model: destinationClaudeModel},
	} {
		conn, _, errDial := websocket.DefaultDialer.Dial(wsURL+test.path, http.Header{"Authorization": []string{"Bearer test-key"}})
		if errDial != nil {
			t.Fatalf("dial %s: %v", test.path, errDial)
		}
		turn := []byte(`{"type":"response.create","model":"` + test.model + `","input":[{"type":"message","role":"user","content":"hi"}]}`)
		if errWrite := conn.WriteMessage(websocket.TextMessage, turn); errWrite != nil {
			t.Fatalf("write %s: %v", test.path, errWrite)
		}
		_, message, errRead := conn.ReadMessage()
		_ = conn.Close()
		if errRead != nil {
			t.Fatalf("read %s: %v", test.path, errRead)
		}
		if test.wantStatus != 0 {
			if gjson.GetBytes(message, "type").String() != "error" || int(gjson.GetBytes(message, "status").Int()) != test.wantStatus {
				t.Fatalf("websocket %s %s event = %s, want error %d", test.path, test.model, message, test.wantStatus)
			}
			continue
		}
		if got := gjson.GetBytes(message, "type").String(); got != "response.completed" {
			t.Fatalf("websocket %s %s event = %s, want response.completed", test.path, test.model, message)
		}
	}
}

func TestDefaultDestinationAllowsAnthropicModelsWhenNotRestricted(t *testing.T) {
	server, claudeExec, _ := newDestinationTestServer(t, false)
	for _, test := range []struct{ path, body string }{
		{path: "/v1/messages", body: messagesBody(destinationClaudeModel)},
		{path: "/v1/messages/count_tokens", body: messagesBody(destinationClaudeModel)},
		{path: "/v1/responses", body: `{"model":"` + destinationClaudeModel + `","input":"hi"}`},
	} {
		if rr := serveDestination(t, server, http.MethodPost, test.path, test.body, nil); rr.Code != http.StatusOK {
			t.Fatalf("%s status = %d, want 200; body=%s", test.path, rr.Code, rr.Body.String())
		}
	}
	if claudeExec.calls() != 3 {
		t.Fatalf("Claude executor calls = %d, want 3", claudeExec.calls())
	}

	// Enabling the flag through hot reload takes effect without a restart.
	setRestrictAnthropicModels(server, true)
	if rr := serveDestination(t, server, http.MethodPost, "/v1/messages", messagesBody(destinationClaudeModel), nil); rr.Code != http.StatusForbidden {
		t.Fatalf("after reload status = %d, want 403", rr.Code)
	}
}

// asClaudeCodeDestination marks req as received through /claude-code, where every model is listed.
func asClaudeCodeDestination(req *http.Request) *http.Request {
	return req.WithContext(handlers.WithClaudeCodeDestination(req.Context()))
}
