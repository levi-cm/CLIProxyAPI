package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"runtime"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

type fixtureCapacityError struct {
	delay time.Duration
	safe  bool
}

func (e fixtureCapacityError) Error() string {
	return `{"error":{"code":"turn_rate_limit_exceeded","message":"Codex is at capacity, please try again later"}}`
}
func (e fixtureCapacityError) StatusCode() int         { return http.StatusTooManyRequests }
func (e fixtureCapacityError) IsCapacity() bool        { return true }
func (e fixtureCapacityError) CapacityRetrySafe() bool { return e.safe }
func (e fixtureCapacityError) RetryAfter() *time.Duration {
	if e.delay < 0 {
		return nil
	}
	d := e.delay
	return &d
}

func capacityFixture(t *testing.T) (*Manager, *Auth, context.Context, cliproxyexecutor.Request, cliproxyexecutor.Options) {
	t.Helper()
	m := NewManager(nil, nil, nil)
	cfg := &config.Config{}
	cfg.Codex.CapacityRetry.Enabled = true
	m.SetConfig(cfg)
	m.capacity.availableMemory = func() (int64, error) { return 5 << 30, nil }
	a, err := m.Register(context.Background(), &Auth{ID: "fixture-a", Provider: "codex", Status: StatusActive, Metadata: map[string]any{"account_id": "fixture-account"}})
	if err != nil {
		t.Fatal(err)
	}
	opts := bindingOptions("fixture-session", "fixture-caller", `{"input":"hello"}`)
	req := cliproxyexecutor.Request{Model: "fixture-model", Payload: opts.OriginalRequest}
	return m, a, m.withCapacityRecovery(context.Background()), req, opts
}

func TestCapacityRecoveryDeadlineAndAttempts(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, a, ctx, req, opts := capacityFixture(t)
		start := time.Now()
		calls := 0
		_, err := runCapacityRecovery(ctx, m, a, req, opts, func(_ context.Context, _ *Auth) (int, error) {
			calls++
			if calls > 1 && time.Since(start) < time.Duration(calls-1)*120*time.Second {
				t.Fatal("retried before upstream deadline")
			}
			return 0, fixtureCapacityError{delay: 120 * time.Second, safe: true}
		})
		if calls != 8 || !isCapacityError(err) {
			t.Fatalf("attempts=%d err=%v", calls, err)
		}
		if time.Since(start) != 14*time.Minute {
			t.Fatalf("elapsed=%v", time.Since(start))
		}
		if m.capacity.pending != 0 || m.capacity.pendingBytes != 0 {
			t.Fatal("reservation leaked")
		}
	})
}

func TestCapacityBudgetNeverRetriesEarly(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, a, ctx, req, opts := capacityFixture(t)
		calls := 0
		_, err := runCapacityRecovery(ctx, m, a, req, opts, func(context.Context, *Auth) (int, error) {
			calls++
			return 0, fixtureCapacityError{delay: 31 * time.Minute, safe: true}
		})
		if calls != 1 || !isCapacityError(err) {
			t.Fatalf("calls=%d err=%v", calls, err)
		}
		if hint := retryAfterFromError(err); hint == nil || *hint != 31*time.Minute {
			t.Fatalf("remaining hint=%v", hint)
		}
	})
}

func TestCapacityCancellationReleasesReservations(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, a, base, req, opts := capacityFixture(t)
		ctx, cancel := context.WithCancel(base)
		done := make(chan error, 1)
		go func() {
			_, err := runCapacityRecovery(ctx, m, a, req, opts, func(context.Context, *Auth) (int, error) {
				return 0, fixtureCapacityError{delay: 120 * time.Second, safe: true}
			})
			done <- err
		}()
		synctest.Wait()
		if m.capacity.pending != 1 {
			t.Fatalf("pending=%d", m.capacity.pending)
		}
		if got := m.PolicyRuntime(a.ID).ActiveRequests; got != 0 {
			t.Fatalf("active while waiting=%d", got)
		}
		cancel()
		synctest.Wait()
		if !errors.Is(<-done, context.Canceled) {
			t.Fatal("cancellation lost")
		}
		if m.capacity.pending != 0 || m.capacity.pendingBytes != 0 || len(m.capacity.sessions) != 0 {
			t.Fatal("reservation leaked")
		}
	})
}

func TestCapacityAdmissionLimits(t *testing.T) {
	c := newCapacityCoordinator()
	c.availableMemory = func() (int64, error) { return 5 << 30, nil }
	cfg := config.CodexCapacityRetryConfig{}.WithDefaults()
	var leases []*capacityReservation
	for i := 0; i < 16; i++ {
		account := "a"
		if i >= 8 {
			account = "b"
		}
		lease := c.reserve(cfg, account, string(rune('a'+i)), 1<<20)
		if lease == nil {
			t.Fatalf("admission %d refused", i)
		}
		leases = append(leases, lease)
	}
	if c.reserve(cfg, "c", "extra", 1) == nil {
	} else {
		t.Fatal("global limit ignored")
	}
	leases[0].release()
	if c.reserve(cfg, "b", "extra", 1) != nil {
		t.Fatal("per-account limit ignored")
	}
	if c.reserve(cfg, "c", "b", 1) != nil {
		t.Fatal("duplicate session admitted")
	}
	for _, lease := range leases {
		lease.release()
	}
	if c.pending != 0 || c.pendingBytes != 0 {
		t.Fatal("release is not idempotent")
	}
	if c.reserve(cfg, "a", "large", 129<<20) != nil {
		t.Fatal("memory budget ignored")
	}
	c.availableMemory = func() (int64, error) { return 900 << 20, nil }
	if c.reserve(cfg, "a", "browser", 1) != nil {
		t.Fatal("browser memory pressure ignored")
	}
	c.availableMemory = func() (int64, error) { return 0, errors.New("measurement failed") }
	if c.reserve(cfg, "a", "unknown", 1) != nil {
		t.Fatal("failed measurement admitted wait")
	}
}

func TestCapacitySingleProbe(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, a, _, req, opts := capacityFixture(t)
		var calls atomic.Int32
		probe := make(chan struct{})
		release := make(chan struct{})
		done := make(chan error, 2)
		attempt := func(context.Context, *Auth) (int, error) {
			n := calls.Add(1)
			if n == 1 {
				return 0, fixtureCapacityError{delay: time.Minute, safe: true}
			}
			if n == 2 {
				close(probe)
				<-release
			}
			return 1, nil
		}
		go func() {
			_, err := runCapacityRecovery(m.withCapacityRecovery(context.Background()), m, a, req, opts, attempt)
			done <- err
		}()
		synctest.Wait()
		opts2 := bindingOptions("second-session", "fixture-caller", `{"input":"hello"}`)
		go func() {
			_, err := runCapacityRecovery(m.withCapacityRecovery(context.Background()), m, a, req, opts2, attempt)
			done <- err
		}()
		synctest.Wait()
		if calls.Load() != 1 {
			t.Fatalf("new arrival ignored admission deadline: calls=%d", calls.Load())
		}
		time.Sleep(time.Minute)
		synctest.Wait()
		<-probe
		if calls.Load() != 2 {
			t.Fatal("parallel recovery probes")
		}
		close(release)
		synctest.Wait()
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		if calls.Load() != 3 {
			t.Fatalf("calls=%d", calls.Load())
		}
	})
}

func TestCapacityUnsafeTurnNeverReplays(t *testing.T) {
	m, a, ctx, req, opts := capacityFixture(t)
	calls := 0
	_, err := runCapacityRecovery(ctx, m, a, req, opts, func(context.Context, *Auth) (int, error) {
		calls++
		return 0, fixtureCapacityError{delay: 0, safe: false}
	})
	if calls != 1 || !isCapacityError(err) || m.capacity.pending != 0 {
		t.Fatalf("calls=%d err=%v", calls, err)
	}
}

func TestCapacityRecoveryPublicManagerStream(t *testing.T) {
	for _, firstChunk := range []bool{false, true} {
		t.Run(fmt.Sprint(firstChunk), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				m, _, _, _, _ := capacityFixture(t)
				m.SetRetryConfig(30, 30, 8)
				ids := registerOverloadAuths(t, m, 2)
				var selected []string
				m.RegisterExecutor(&customStreamMockExecutor{identifier: "codex", streamFn: func(_ context.Context, a *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
					selected = append(selected, a.ID)
					if len(selected) == 1 {
						err := fixtureCapacityError{delay: 120 * time.Second, safe: true}
						if firstChunk {
							return streamErrorResult(nil, err), nil
						}
						return nil, err
					}
					return successStreamResult(), nil
				}})
				opts := bindingOptions("public-session", "fixture-caller", `{"input":"hello"}`)
				start := time.Now()
				result, err := m.ExecuteStream(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: "gpt-5.6-terra", Payload: opts.OriginalRequest}, opts)
				if err != nil {
					t.Fatal(err)
				}
				for chunk := range result.Chunks {
					if chunk.Err != nil {
						t.Fatal(chunk.Err)
					}
				}
				if len(selected) != 2 || selected[0] != ids[0] || selected[1] != ids[0] || time.Since(start) != 120*time.Second {
					t.Fatalf("order=%v elapsed=%v", selected, time.Since(start))
				}
				if a, _ := m.GetByID(ids[0]); a.Quota.Exceeded || a.ModelStates["gpt-5.6-terra"].Quota.Exceeded {
					t.Fatal("capacity marked quota exhausted")
				}
				if m.capacity.pending != 0 {
					t.Fatal("pending leaked")
				}
			})
		})
	}
}

func TestCapacityCumulativeBudget(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, a, ctx, req, opts := capacityFixture(t)
		calls := 0
		start := time.Now()
		_, err := runCapacityRecovery(ctx, m, a, req, opts, func(context.Context, *Auth) (int, error) {
			calls++
			return 0, fixtureCapacityError{delay: 7 * time.Minute, safe: true}
		})
		if calls != 5 || time.Since(start) != 28*time.Minute || !isCapacityFinal(err) {
			t.Fatalf("calls=%d elapsed=%v err=%v", calls, time.Since(start), err)
		}
	})
}

func TestCapacityAdmissionRefusalStopsOuterRetry(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, _, _, _, _ := capacityFixture(t)
		m.SetRetryConfig(30, 30, 8)
		registerOverloadAuths(t, m, 2)
		m.capacity.availableMemory = func() (int64, error) { return 900 << 20, nil }
		calls := 0
		m.RegisterExecutor(&customStreamMockExecutor{identifier: "codex", streamFn: func(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
			calls++
			return nil, fixtureCapacityError{delay: 120 * time.Second, safe: true}
		}})
		opts := bindingOptions("public-session", "fixture-caller", `{"input":"hello"}`)
		_, err := m.ExecuteStream(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: "gpt-5.6-terra", Payload: opts.OriginalRequest}, opts)
		if calls != 1 || !isCapacityFinal(err) || SafeResponseHeaders(err).Get("Retry-After") != "120" {
			t.Fatalf("calls=%d err=%v headers=%v", calls, err, SafeResponseHeaders(err))
		}
	})
}

func TestCapacityFallbackDelayBounds(t *testing.T) {
	for n := 1; n <= 10; n++ {
		base := 30 * time.Second
		for i := 1; i < n && base < 5*time.Minute; i++ {
			base *= 2
		}
		if base > 5*time.Minute {
			base = 5 * time.Minute
		}
		for range 100 {
			d := capacityFallbackDelay(n)
			if d < base || d > min(5*time.Minute, base+base/20) {
				t.Fatalf("n=%d delay=%v", n, d)
			}
		}
	}
}

func TestCapacityCredentialRevalidation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, a, ctx, req, opts := capacityFixture(t)
		calls := 0
		done := make(chan error, 1)
		go func() {
			_, err := runCapacityRecovery(ctx, m, a, req, opts, func(context.Context, *Auth) (int, error) {
				calls++
				return 0, fixtureCapacityError{delay: time.Minute, safe: true}
			})
			done <- err
		}()
		synctest.Wait()
		changed := a.Clone()
		changed.Metadata["account_id"] = "different-account"
		if _, err := m.Update(context.Background(), changed); err != nil {
			t.Fatal(err)
		}
		time.Sleep(time.Minute)
		synctest.Wait()
		if !isCapacityFinal(<-done) || calls != 1 || m.capacity.pending != 0 {
			t.Fatal("identity changed during wait but recovery executed or leaked")
		}
	})
}

func TestCapacitySaturatedQueueMemoryWithBrowser(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, a, _, _, _ := capacityFixture(t)
		b, err := m.Register(context.Background(), &Auth{ID: "fixture-b", Provider: "codex", Status: StatusActive, Metadata: map[string]any{"account_id": "second-account"}})
		if err != nil {
			t.Fatal(err)
		}
		// Model the measured 5.5 GiB host headroom after an additional 3 GiB browser.
		m.capacity.availableMemory = func() (int64, error) { return (5 << 30) + (512 << 20) - (3 << 30), nil }
		runtime.GC()
		var before runtime.MemStats
		runtime.ReadMemStats(&before)
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 16)
		for i := 0; i < 16; i++ {
			auth := a
			if i >= 8 {
				auth = b
			}
			opts := bindingOptions(fmt.Sprintf("saturated-%d", i), "fixture-caller", `{"input":"hello"}`)
			req := cliproxyexecutor.Request{Model: "fixture-model", Payload: make([]byte, 256<<10)}
			go func() {
				_, err := runCapacityRecovery(m.withCapacityRecovery(ctx), m, auth, req, opts, func(context.Context, *Auth) (int, error) {
					return 0, fixtureCapacityError{delay: time.Hour, safe: true}
				})
				done <- err
			}()
		}
		// A hint beyond budget is declined immediately; use an in-budget deadline
		// to prove a saturated retained queue below.
		for range 16 {
			<-done
		}
		if m.capacity.pending != 0 {
			t.Fatal("over-budget reservations retained")
		}
		m.capacity.gates = make(map[string]*capacityGate)
		for i := 0; i < 16; i++ {
			auth := a
			if i >= 8 {
				auth = b
			}
			opts := bindingOptions(fmt.Sprintf("saturated-%d", i), "fixture-caller", `{"input":"hello"}`)
			req := cliproxyexecutor.Request{Model: "fixture-model", Payload: make([]byte, 256<<10)}
			go func() {
				_, err := runCapacityRecovery(m.withCapacityRecovery(ctx), m, auth, req, opts, func(context.Context, *Auth) (int, error) {
					return 0, fixtureCapacityError{delay: time.Minute, safe: true}
				})
				done <- err
			}()
		}
		synctest.Wait()
		runtime.GC()
		var saturated runtime.MemStats
		runtime.ReadMemStats(&saturated)
		status := m.CapacityRecoveryStatus()
		retained := int64(saturated.HeapAlloc) - int64(before.HeapAlloc)
		t.Logf("pending=%d accounted=%d retained_heap_delta=%d", status.Pending, status.PendingBytes, retained)
		if status.Pending != 16 || status.PendingBytes > 128<<20 || retained > status.PendingBytes {
			t.Fatalf("unsafe saturated queue: %+v heap=%d", status, retained)
		}
		cancel()
		synctest.Wait()
		for range 16 {
			if err := <-done; !errors.Is(err, context.Canceled) {
				t.Fatalf("cancellation: %v", err)
			}
		}
		if status := m.CapacityRecoveryStatus(); status.Pending != 0 || status.PendingBytes != 0 {
			t.Fatal("memory reservation leak")
		}
	})
}

func TestCapacityRecoveryPublicManagerExecute(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, _, _, _, _ := capacityFixture(t)
		m.SetRetryConfig(30, 30, 8)
		ids := registerOverloadAuths(t, m, 2)
		var order []string
		m.RegisterExecutor(&mockCustomErrorExecutor{identifier: "codex", executeFn: func(_ context.Context, a *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
			order = append(order, a.ID)
			if len(order) == 1 {
				return cliproxyexecutor.Response{}, fixtureCapacityError{delay: 120 * time.Second, safe: true}
			}
			return cliproxyexecutor.Response{Payload: []byte(`{"ok":true}`)}, nil
		}})
		opts := bindingOptions("execute-session", "fixture-caller", `{"input":"hello"}`)
		start := time.Now()
		_, err := m.Execute(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: "gpt-5.6-terra", Payload: opts.OriginalRequest}, opts)
		if err != nil || len(order) != 2 || order[0] != ids[0] || order[1] != ids[0] || time.Since(start) != 2*time.Minute {
			t.Fatalf("order=%v elapsed=%v err=%v", order, time.Since(start), err)
		}
	})
}

func TestCapacityDisabledDoesNotRetryEarly(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, _, _, _, _ := capacityFixture(t)
		m.SetRetryConfig(30, 30, 8)
		registerOverloadAuths(t, m, 2)
		cfg := &config.Config{}
		m.SetConfig(cfg)
		calls := 0
		m.RegisterExecutor(&mockCustomErrorExecutor{identifier: "codex", executeFn: func(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
			calls++
			return cliproxyexecutor.Response{}, fixtureCapacityError{delay: 120 * time.Second, safe: true}
		}})
		opts := bindingOptions("execute-session", "fixture-caller", `{"input":"hello"}`)
		_, err := m.Execute(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: "gpt-5.6-terra", Payload: opts.OriginalRequest}, opts)
		if calls != 1 || !isCapacityError(err) {
			t.Fatalf("disabled recovery retried without waiting: calls=%d err=%v", calls, err)
		}
	})
}
