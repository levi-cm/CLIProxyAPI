package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/subagentadvice"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// historyInventory adds durable long-window history, which in production
// comes from the usage ledger and the sampler.
type historyInventory struct {
	inner   subagentadvice.InventorySource
	history map[string][]subagentadvice.Sample
}

func (h historyInventory) Inventory(ctx context.Context) (subagentadvice.Inventory, error) {
	inv, err := h.inner.Inventory(ctx)
	for i := range inv.Accounts {
		inv.Accounts[i].History = h.history[inv.Accounts[i].ID]
	}
	return inv, err
}

type adviceFixture struct {
	server *Server
}

func claudeSignals(used float64, reset, observed time.Time) map[string]string {
	return map[string]string{
		"Anthropic-Ratelimit-Unified-7d-Reset":       strconv.FormatInt(reset.Unix(), 10),
		"Anthropic-Ratelimit-Unified-7d-Utilization": strconv.FormatFloat(used, 'f', 2, 64),
		"Anthropic-Ratelimit-Unified-5h-Reset":       strconv.FormatInt(observed.Add(3*time.Hour).Unix(), 10),
		"Anthropic-Ratelimit-Unified-5h-Utilization": "0.10",
	}
}

// newAdviceFixture registers two Claude accounts (claude-b idle with 60% left
// and a reset in 10h, so it has surplus) and two Codex accounts without quota
// telemetry. Claude accounts also serve Fable, which must never be advised.
func newAdviceFixture(t *testing.T) *adviceFixture {
	t.Helper()
	gin.SetMode(gin.TestMode)
	cfg := &config.Config{}
	m := auth.NewManager(nil, nil, nil)
	m.SetConfig(cfg)
	levels := []string{"low", "medium", "high", "xhigh", "max"}
	now := time.Now().UTC()
	reset := map[string]time.Time{"claude-a": now.Add(96 * time.Hour), "claude-b": now.Add(10 * time.Hour)}
	used := map[string]float64{"claude-a": 0.5, "claude-b": 0.4}
	reg := registry.GetGlobalRegistry()
	for _, id := range []string{"claude-a", "claude-b"} {
		a := &auth.Auth{ID: id, Provider: "claude", Attributes: map[string]string{"api_key": "key-" + id}, Quota: auth.QuotaState{ObservedAt: now, Signals: claudeSignals(used[id], reset[id], now)}}
		if _, err := m.Register(context.Background(), a); err != nil {
			t.Fatal(err)
		}
		reg.RegisterClient(id, "claude", []*registry.ModelInfo{
			{ID: "claude-opus-5-5", Type: "claude", OwnedBy: "anthropic", ContextLength: 1000000, MaxCompletionTokens: 128000, Thinking: &registry.ThinkingSupport{Levels: levels}},
			{ID: "claude-fable-5-2", Type: "claude", OwnedBy: "anthropic", ContextLength: 1000000, MaxCompletionTokens: 128000, Thinking: &registry.ThinkingSupport{Levels: levels}},
		})
	}
	for _, id := range []string{"codex-a", "codex-b"} {
		a := &auth.Auth{ID: id, Provider: "codex", Attributes: map[string]string{"api_key": "key-" + id}}
		if _, err := m.Register(context.Background(), a); err != nil {
			t.Fatal(err)
		}
		reg.RegisterClient(id, "codex", []*registry.ModelInfo{{ID: "gpt-6.1-sol", Type: "openai", OwnedBy: "openai", ContextLength: 272000, MaxCompletionTokens: 128000, Thinking: &registry.ThinkingSupport{Levels: levels}}})
	}
	t.Cleanup(func() {
		for _, id := range []string{"claude-a", "claude-b", "codex-a", "codex-b"} {
			reg.UnregisterClient(id)
		}
	})
	s := NewServer(cfg, m, nil, "", WithRequestLoggerFactory(nil), WithMiddleware(func(c *gin.Context) {
		if key := strings.TrimPrefix(c.GetHeader("Authorization"), "Bearer "); key != "" {
			c.Set("userApiKey", key)
		}
	}))
	history := map[string][]subagentadvice.Sample{}
	for _, id := range []string{"claude-a", "claude-b"} {
		for h := 6; h >= 1; h-- {
			history[id] = append(history[id], subagentadvice.Sample{At: now.Add(-time.Duration(h) * time.Hour), UsedPercent: used[id] * 100, ResetAt: time.Unix(reset[id].Unix(), 0).UTC()})
		}
	}
	s.subagentAdvisor = subagentadvice.New(cfg.SubagentAdvisor, historyInventory{inner: newSubagentInventory(m, nil, nil), history: history})
	return &adviceFixture{server: s}
}

func (fx *adviceFixture) get(t *testing.T, method, path, key string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	w := httptest.NewRecorder()
	fx.server.Handler().ServeHTTP(w, req)
	return w
}

func TestSubagentAdviceIsKeylessAndReadOnly(t *testing.T) {
	fx := newAdviceFixture(t)
	var first subagentadvice.Advice
	for _, key := range []string{"", "caller-a"} {
		w := fx.get(t, http.MethodGet, subagentAdvicePath, key)
		if w.Code != http.StatusOK || w.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("advice (key %q): %d %s", key, w.Code, w.Body.String())
		}
		var a subagentadvice.Advice
		if err := json.Unmarshal(w.Body.Bytes(), &a); err != nil || a.Schema != subagentadvice.Schema {
			t.Fatalf("advice body: %v %s", err, w.Body.String())
		}
		if key == "" {
			first = a
		} else if a.Posture != first.Posture || a.Summary != first.Summary {
			t.Fatalf("a key changed the advice: %s vs %s", a.Summary, first.Summary)
		}
		for _, leaked := range []string{"claude-a", "claude-b", "codex-a", "key-claude", "acct_"} {
			if strings.Contains(w.Body.String(), leaked) {
				t.Fatalf("advice exposes %q: %s", leaked, w.Body.String())
			}
		}
	}
	// The per-account forecast is diagnostic detail and needs inference auth.
	if w := fx.get(t, http.MethodGet, subagentForecastPath, ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("keyless forecast: %d %s", w.Code, w.Body.String())
	}
	if w := fx.get(t, http.MethodGet, subagentForecastPath, "caller-a"); w.Code != http.StatusOK || strings.Contains(w.Body.String(), "claude-b\"") || !strings.Contains(w.Body.String(), `"capability":"claude-opus-5.5"`) {
		t.Fatalf("forecast: %d %s", w.Code, w.Body.String())
	}
	if w := fx.get(t, http.MethodGet, subagentAdvicePath+"?context_tokens=abc", ""); w.Code != http.StatusBadRequest {
		t.Fatalf("invalid query accepted: %d", w.Code)
	}
	if w := fx.get(t, http.MethodGet, subagentAdvicePath+"?format=xml", ""); w.Code != http.StatusBadRequest {
		t.Fatalf("unknown format accepted: %d", w.Code)
	}
	if w := fx.get(t, http.MethodGet, subagentAdvicePath+"?format=text", ""); w.Code != http.StatusOK || !strings.HasPrefix(w.Body.String(), "spend freely: ") || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/plain") {
		t.Fatalf("text format: %d %q", w.Code, w.Body.String())
	}
	if w := fx.get(t, http.MethodGet, subagentAdvicePath+"?context_tokens=-1", ""); w.Code != http.StatusBadRequest {
		t.Fatalf("negative context_tokens accepted: %d", w.Code)
	}
	// Nothing writes: other methods are not routed, and the old plan API is gone.
	for _, test := range []struct{ method, path string }{
		{http.MethodPost, subagentAdvicePath},
		{http.MethodDelete, subagentAdvicePath},
		{http.MethodPost, "/v1/account-policy/review-plans"},
	} {
		if w := fx.get(t, test.method, test.path, "caller-a"); w.Code == http.StatusOK || w.Code == http.StatusCreated {
			t.Fatalf("%s %s returned %d", test.method, test.path, w.Code)
		}
	}
	// The rest of the account-policy client API still requires a key.
	if w := fx.get(t, http.MethodGet, "/v1/account-policy/usage", ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("keyed client route opened: %d", w.Code)
	}
}

func TestSubagentAdviceUsesSurplusAndNeverFable(t *testing.T) {
	fx := newAdviceFixture(t)
	w := fx.get(t, http.MethodGet, subagentAdvicePath, "")
	var a subagentadvice.Advice
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &a) != nil {
		t.Fatalf("advice: %d %s", w.Code, w.Body.String())
	}
	if a.Status != subagentadvice.StatusReady || a.Posture != subagentadvice.PostureSpendFreely || len(a.Models) != 2 {
		t.Fatalf("advice = %s %s %+v", a.Status, a.Posture, a.Models)
	}
	opus, sol := a.Models[0], a.Models[1]
	if opus.Model != "claude-opus-5-5" || opus.ReasoningEffort != "xhigh" || opus.Mode != subagentadvice.ModeFast || opus.Basis != subagentadvice.KindSurplus || opus.Harnesses["claude-code"] != "opus" || opus.BudgetTasks == nil || *opus.BudgetTasks < 100 {
		t.Fatalf("opus = %+v", opus)
	}
	if sol.Model != "gpt-6.1-sol" || sol.ReasoningEffort != "high" || sol.Harnesses["codex"] != "gpt-6.1-sol" || sol.BudgetTasks != nil {
		t.Fatalf("sol = %+v", sol)
	}
	for _, limit := range []string{"max_parallel", `"count"`} {
		if strings.Contains(w.Body.String(), limit) {
			t.Fatalf("advice carries a subagent limit %s: %s", limit, w.Body.String())
		}
	}
	if strings.Contains(w.Body.String(), "fable") {
		t.Fatalf("fable must never be advised: %s", w.Body.String())
	}
}

func TestSubagentInventoryReflectsAccountChanges(t *testing.T) {
	fx := newAdviceFixture(t)
	inv, err := newSubagentInventory(nil, nil, nil).Inventory(context.Background())
	if err == nil || len(inv.Accounts) != 0 {
		t.Fatalf("nil manager must fail closed")
	}
	source := newSubagentInventory(fx.server.handlers.AuthManager, nil, nil)
	inv, err = source.Inventory(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]subagentadvice.Account{}
	for _, a := range inv.Accounts {
		byID[a.ID] = a
	}
	b := byID["claude-b"]
	if len(b.Windows) != 2 || b.Windows[0].Name != "7d" || b.Windows[0].UsedPercent != 40 || !b.Windows[0].Long || b.Windows[1].Name != "5h" {
		t.Fatalf("claude windows = %+v", b.Windows)
	}
	if len(byID["codex-a"].Windows) != 0 || len(byID["codex-a"].Models) != 1 {
		t.Fatalf("codex without policy snapshot should be unmetered: %+v", byID["codex-a"])
	}
	// A disabled account is reported as blocked.
	m := fx.server.handlers.AuthManager
	a, _ := m.GetByID("codex-a")
	a.Disabled = true
	a.Status = auth.StatusDisabled
	if _, err = m.Update(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	inv, _ = source.Inventory(context.Background())
	for _, acct := range inv.Accounts {
		if acct.ID == "codex-a" && acct.Blocked != "disabled" {
			t.Fatalf("disabled account = %+v", acct)
		}
	}
}

func TestUsageSplitSkillReadsAdviceWithoutKey(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not available")
	}
	if _, err = exec.LookPath("curl"); err != nil {
		t.Skip("curl not available")
	}
	script, err := filepath.Abs(filepath.Join("..", "..", "skills", "usage-split", "scripts", "advise.sh"))
	if err != nil {
		t.Fatal(err)
	}
	fx := newAdviceFixture(t)
	srv := newTestHTTPServer(t, fx.server.Handler())
	run := func(base string, args ...string) (string, string, error) {
		cmd := exec.Command(bash, append([]string{script}, args...)...)
		// No key anywhere in the environment: the advice is keyless.
		cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + t.TempDir(), "USAGE_SPLIT_URL=" + base}
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		err := cmd.Run()
		return stdout.String(), stderr.String(), err
	}
	out, stderr, err := run(srv.URL)
	if err != nil {
		t.Fatalf("advise.sh failed: %v\n%s", err, stderr)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 4 || lines[3] != "warning: deepseek-4.1 unavailable: model_not_available" || !strings.HasPrefix(lines[0], "spend freely: allowance would expire unused, so many parallel subagents and fast mode are welcome | reuse until ") {
		t.Fatalf("advice = %q", out)
	}
	if !strings.HasPrefix(lines[1], "claude-opus-5-5 effort=xhigh mode=fast | ~") || !strings.Contains(lines[1], " subagent tasks before reset in 9h | claude-code: opus") || lines[2] != "gpt-6.1-sol effort=high mode=standard | budget unknown | codex: gpt-6.1-sol, opencode: gpt-6.1-sol" {
		t.Fatalf("subagent lines = %q", lines[1:])
	}
	if strings.Contains(strings.ToLower(out), "fable") {
		t.Fatalf("fable advised: %s", out)
	}
	// An unreachable proxy is a clear refusal, never a guess.
	srv.Close()
	out, stderr, err = run(srv.URL)
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 3 || out != "" || !strings.Contains(stderr, "do not start extra subagents") {
		t.Fatalf("unreachable: %v %q %q", err, out, stderr)
	}
}

type fakeClaudeLimits map[string]auth.ClaudeLimitWindowState

func (f fakeClaudeLimits) ClaudeLimitWindows(string, uint64) map[string]auth.ClaudeLimitWindowState {
	return f
}

// A stream that started before the 5h window filled can finish later with a
// newer timestamp but older utilization; the selector's full window wins until
// a header reports a later window.
func TestClaudeWindowsKeepSelectorFullWindow(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	reset5h := now.Add(3 * time.Hour)
	a := &auth.Auth{ID: "c", Provider: "claude", Quota: auth.QuotaState{ObservedAt: now, Signals: claudeSignals(0.5, now.Add(96*time.Hour), now)}}
	limits := fakeClaudeLimits{"5h": {Full: true, Reset: reset5h, Observed: now.Add(-time.Minute)}}
	find := func(ws []subagentadvice.Window) subagentadvice.Window {
		for _, w := range ws {
			if w.Name == "5h" {
				return w
			}
		}
		t.Fatalf("no 5h window in %+v", ws)
		return subagentadvice.Window{}
	}
	if w := find(claudeWindows(a, limits)); !w.Full || !w.ResetAt.Equal(reset5h) {
		t.Fatalf("late header reopened the full window: %+v", w)
	}
	// A header for the next window (later reset) supersedes the old state.
	a.Quota.Signals["Anthropic-Ratelimit-Unified-5h-Reset"] = strconv.FormatInt(now.Add(8*time.Hour).Unix(), 10)
	if w := find(claudeWindows(a, limits)); w.Full {
		t.Fatalf("a later window stayed full: %+v", w)
	}
}
