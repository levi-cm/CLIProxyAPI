package cliproxy

import (
	"context"
	"errors"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/accountpolicy"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/accountpolicyusage"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/redisqueue"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
)

type observationUsageExecutor struct{}

func (*observationUsageExecutor) Identifier() string { return "codex" }
func (e *observationUsageExecutor) Execute(ctx context.Context, auth *coreauth.Auth, request coreexecutor.Request, _ coreexecutor.Options) (coreexecutor.Response, error) {
	helps.NewExecutorUsageReporter(ctx, e, request.Model, auth).Publish(ctx, usage.Detail{InputTokens: 7, OutputTokens: 3, TotalTokens: 10})
	return coreexecutor.Response{Payload: []byte(`{"ok":true}`)}, nil
}
func (*observationUsageExecutor) ExecuteStream(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (*coreexecutor.StreamResult, error) {
	return nil, errors.New("streaming not used by this fixture")
}
func (*observationUsageExecutor) Refresh(_ context.Context, auth *coreauth.Auth) (*coreauth.Auth, error) {
	return auth, nil
}
func (*observationUsageExecutor) CountTokens(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error) {
	return coreexecutor.Response{}, errors.New("token counting not used by this fixture")
}
func (*observationUsageExecutor) HttpRequest(context.Context, *coreauth.Auth, *http.Request) (*http.Response, error) {
	return nil, errors.New("HTTP requests not used by this fixture")
}

type observationUsageBarrier chan struct{}

func (b observationUsageBarrier) HandleUsage(context.Context, usage.Record) { b <- struct{}{} }

// Shutdown stops the process-wide usage dispatcher, so isolate this lifecycle
// test from other SDK tests that use that dispatcher.
func TestAccountPolicyObservationUsageLifecycle(t *testing.T) {
	const childEnv = "CLIPROXY_OBSERVATION_USAGE_TEST_CHILD"
	if os.Getenv(childEnv) != "1" {
		command := exec.Command(os.Args[0], "-test.run=^TestAccountPolicyObservationUsageLifecycle$", "-test.v")
		command.Env = append(os.Environ(), childEnv+"=1")
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("observation usage lifecycle: %v\n%s", err, output)
		}
		return
	}

	redisqueue.SetUsageStatisticsEnabled(false)
	selector := &lastPolicySelector{}
	manager := coreauth.NewManager(nil, selector, nil)
	policyTestAuth(t, manager, "observation-credential", "observation-account")
	manager.RegisterExecutor(&observationUsageExecutor{})
	registry.GetGlobalRegistry().RegisterClient("observation-credential", "codex", []*registry.ModelInfo{{ID: "observation-model"}})
	defer registry.GetGlobalRegistry().UnregisterClient("observation-credential")
	cfg := &config.Config{AccountPolicy: accountpolicy.DefaultSettings()}
	cfg.AccountPolicy.StateDir = filepath.Join(t.TempDir(), "state")
	build := func() *Service {
		t.Helper()
		service, err := NewBuilder().WithConfig(cfg).WithConfigPath("config.yaml").WithCoreAuthManager(manager).WithAccountPolicyProvider(&policyFixtureProvider{}).Build()
		if err != nil {
			t.Fatal(err)
		}
		if err = service.startAccountPolicy(context.Background()); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(service.stopAccountPolicy)
		return service
	}
	service := build()
	barrier := make(observationUsageBarrier, 8)
	usage.RegisterNamedPlugin("observation-test-barrier", barrier)
	execute := func(wait bool) {
		t.Helper()
		if _, err := manager.Execute(context.Background(), []string{"codex"}, coreexecutor.Request{Model: "observation-model"}, coreexecutor.Options{}); err != nil {
			t.Fatal(err)
		}
		if wait {
			select {
			case <-barrier:
			case <-time.After(5 * time.Second):
				t.Fatal("usage dispatcher did not finish delivering executor completion")
			}
		}
	}
	assertDashboard := func(sink *accountpolicyusage.Sink, requests uint64, collecting bool) {
		t.Helper()
		snapshot := sink.Dashboard(time.Now().UTC().Add(time.Second), time.Hour)
		if !snapshot.Available || snapshot.Collecting != collecting || snapshot.Totals.Requests != requests {
			t.Fatalf("dashboard requests=%d collecting=%v: %+v", requests, collecting, snapshot)
		}
		if requests > 0 && (snapshot.Totals.TotalTokens == nil || *snapshot.Totals.TotalTokens != int64(requests)*10 || len(snapshot.Accounts) != 1 || snapshot.Accounts[0].CredentialID != "observation-credential") {
			t.Fatalf("executor measurements missing from dashboard: %+v", snapshot)
		}
	}

	execute(true)
	if _, err := os.Stat(cfg.AccountPolicy.StateDir); !os.IsNotExist(err) {
		t.Fatalf("default-disabled collection mutated storage: %v", err)
	}
	settings := service.accountPolicy.Settings()
	settings.ObservationsEnabled = true
	if err := service.accountPolicy.MergeSettings(func(accountpolicy.Settings) (accountpolicy.Settings, error) {
		return settings, service.validateAccountPolicySettings(settings)
	}); err != nil {
		t.Fatal(err)
	}
	execute(true)
	sink := service.accountPolicyUsageSink()
	assertDashboard(sink, 1, true)
	if manager.Selector() != selector || service.accountPolicyRoutingInstalled.Load() || service.accountPolicy.Settings().Enabled {
		t.Fatal("observation-only collection acquired routing ownership")
	}
	if cfg.UsageStatisticsEnabled || redisqueue.UsageStatisticsEnabled() {
		t.Fatal("private observations enabled global upstream usage statistics")
	}
	if _, err := service.accountPolicy.Redeem(context.Background(), "observation-credential", "fixture-credit"); err == nil {
		t.Fatal("observation-only collection granted reset authority")
	}

	settings.ObservationsEnabled = false
	if err := service.accountPolicy.UpdateSettings(settings); err != nil {
		t.Fatal(err)
	}
	execute(true)
	assertDashboard(sink, 1, false)
	settings.ObservationsEnabled = true
	if err := service.accountPolicy.UpdateSettings(settings); err != nil {
		t.Fatal(err)
	}
	service.stopAccountPolicy()
	if err := sink.Close(); err != nil {
		t.Fatal(err)
	}

	cfg.AccountPolicy.ObservationsEnabled = true
	restarted := build()
	restartedSink := restarted.accountPolicyUsageSink()
	assertDashboard(restartedSink, 1, true)
	execute(true)
	assertDashboard(restartedSink, 2, true)
	execute(false)
	restarted.stopAccountPolicy()
	restarted.closeAccountPolicyUsage(context.Background())
	assertDashboard(restartedSink, 3, false)
	if !restartedSink.Summary().Closed {
		t.Fatal("shutdown did not close durable storage after draining usage")
	}
	data, err := os.ReadFile(filepath.Join(cfg.AccountPolicy.StateDir, "usage.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"secret-access", "secret-refresh", "id_token", "access_token", "refresh_token", "access_token_sha256", "api_key", "auth_index", "response_headers"} {
		if strings.Contains(string(data), secret) {
			t.Fatalf("durable usage contains sensitive field or credential material %q", secret)
		}
	}
}

func TestAccountPolicyObservationsPreserveCorruptStateAndNormalProxy(t *testing.T) {
	for _, filename := range []string{"state.json", "usage.jsonl"} {
		t.Run(filename, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, filename)
			corrupt := []byte("unreadable complete record\n")
			if err := os.WriteFile(path, corrupt, 0600); err != nil {
				t.Fatal(err)
			}
			selector := &lastPolicySelector{}
			manager := coreauth.NewManager(nil, selector, nil)
			cfg := &config.Config{AccountPolicy: accountpolicy.DefaultSettings()}
			cfg.AccountPolicy.ObservationsEnabled = true
			cfg.AccountPolicy.StateDir = dir
			service, err := NewBuilder().WithConfig(cfg).WithConfigPath("config.yaml").WithCoreAuthManager(manager).WithAccountPolicyProvider(&policyFixtureProvider{}).Build()
			if err != nil {
				t.Fatalf("optional observations prevented ordinary proxy startup: %v", err)
			}
			if err = service.startAccountPolicy(context.Background()); err != nil {
				t.Fatalf("optional observations prevented ordinary proxy startup: %v", err)
			}
			defer service.stopAccountPolicy()
			if manager.Selector() != selector || cfg.UsageStatisticsEnabled {
				t.Fatal("unavailable observations changed ordinary proxy defaults")
			}
			if filename == "state.json" {
				if service.accountPolicy != nil {
					t.Fatal("unreadable policy journal silently replaced")
				}
			} else {
				sink := service.accountPolicyUsageSink()
				defer func() {
					if errClose := sink.Close(); errClose != nil {
						t.Error(errClose)
					}
				}()
				if snapshot := sink.Dashboard(time.Now().UTC(), time.Hour); snapshot.Available {
					t.Fatalf("corrupt usage supplied invented dashboard evidence: %+v", snapshot)
				}
				sink.HandleUsage(context.Background(), usage.Record{RequestedAt: time.Now().UTC(), Detail: usage.Detail{TotalTokens: 10}})
				if sink.Summary().WriteErrors != 1 {
					t.Fatal("corrupt usage was silently accepted for writing")
				}
			}
			saved, err := os.ReadFile(path)
			if err != nil || string(saved) != string(corrupt) {
				t.Fatalf("corrupt observation state overwritten: %v", err)
			}
		})
	}
}

func TestAccountPolicyRoutingEnabledStillCollectsUsage(t *testing.T) {
	cfg := &config.Config{AccountPolicy: accountpolicy.DefaultSettings()}
	cfg.AccountPolicy.Enabled = true
	cfg.AccountPolicy.StateDir = t.TempDir()
	service, err := NewBuilder().WithConfig(cfg).WithConfigPath("config.yaml").WithAccountPolicyProvider(&policyFixtureProvider{}).Build()
	if err != nil {
		t.Fatal(err)
	}
	if err = service.startAccountPolicy(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer service.stopAccountPolicy()
	sink := service.accountPolicyUsageSink()
	defer func() {
		if errClose := sink.Close(); errClose != nil {
			t.Error(errClose)
		}
	}()
	now := time.Now().UTC()
	sink.HandleUsage(context.Background(), usage.Record{RequestedAt: now, Detail: usage.Detail{TotalTokens: 10}})
	if snapshot := sink.Dashboard(now, time.Hour); !snapshot.Collecting || snapshot.Totals.Requests != 1 {
		t.Fatalf("routing-enabled policy stopped collecting usage: %+v", snapshot)
	}
}
