package auth

import (
	"context"
	"net/http"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
)

func TestPolicyRefreshEventsOnlyQuotaAndCredentialMaterialChanges(t *testing.T) {
	m := NewManager(nil, nil, nil)
	_, err := m.Register(WithSkipPersist(context.Background()), &Auth{ID: "a", Provider: "codex", Metadata: map[string]any{"access_token": "old", "account_id": "account", "workspace_id": "workspace"}})
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	m.SetPolicyRefreshCallback(func(id string) { ids = append(ids, id) })
	m.MarkResult(context.Background(), Result{AuthID: "a", Provider: "codex", Success: true})
	m.MarkResult(context.Background(), Result{AuthID: "a", Provider: "codex", Error: &Error{HTTPStatus: 500}})
	if len(ids) != 0 {
		t.Fatalf("ordinary inference caused provider discovery: %v", ids)
	}
	m.MarkResult(context.Background(), Result{AuthID: "a", Provider: "codex", Error: &Error{HTTPStatus: 429}})
	if len(ids) != 1 || ids[0] != "a" {
		t.Fatalf("quota event refresh=%v", ids)
	}
	a, _ := m.GetByID("a")
	a.Label = "new alias"
	if _, err = m.Update(WithSkipPersist(context.Background()), a); err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 {
		t.Fatalf("display alias change caused discovery: %v", ids)
	}
	a, _ = m.GetByID("a")
	a.Metadata["access_token"] = "new"
	if _, err = m.UpdateRefreshedAuth(WithSkipPersist(context.Background()), m.auths["a"].Clone(), a); err != nil {
		t.Fatal(err)
	}
	if len(ids) != 2 {
		t.Fatalf("credential refresh did not enqueue discovery: %v", ids)
	}
	a, _ = m.GetByID("a")
	a.Metadata["account_id"] = "replacement"
	if _, err = m.Update(WithSkipPersist(context.Background()), a); err != nil {
		t.Fatal(err)
	}
	if len(ids) != 3 {
		t.Fatalf("account identity change did not enqueue discovery: %v", ids)
	}
}

func TestPolicyPassiveDepletionEnqueuesOneRefresh(t *testing.T) {
	m := NewManager(nil, nil, nil)
	_, err := m.Register(WithSkipPersist(context.Background()), &Auth{ID: "a", Provider: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	m.SetPolicyRefreshCallback(func(string) { count++ })
	ctx := logging.WithResponseHeadersHolder(context.Background())
	logging.SetResponseHeaders(ctx, http.Header{"X-Codex-Primary-Window-Minutes": []string{"10080"}, "X-Codex-Primary-Used-Percent": []string{"30"}})
	m.MarkResult(ctx, Result{AuthID: "a", Provider: "codex", Success: true})
	if count != 0 {
		t.Fatal("ordinary successful quota observation polled provider")
	}
	logging.SetResponseHeaders(ctx, http.Header{"X-Codex-Primary-Window-Minutes": []string{"10080"}, "X-Codex-Primary-Used-Percent": []string{"100"}})
	m.MarkResult(ctx, Result{AuthID: "a", Provider: "codex", Success: true})
	m.MarkResult(ctx, Result{AuthID: "a", Provider: "codex", Success: true})
	if count != 1 {
		t.Fatalf("depletion transition caused %d refreshes, want one", count)
	}
}
