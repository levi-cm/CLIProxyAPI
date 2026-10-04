package usage

import (
	"context"
	"errors"
	"sync"
	"testing"
)

type blockedUsagePlugin struct {
	entered chan struct{}
	release chan struct{}
	mu      sync.Mutex
	models  []string
}

func (p *blockedUsagePlugin) HandleUsage(_ context.Context, record Record) {
	if record.Model == "first" {
		close(p.entered)
		<-p.release
	}
	p.mu.Lock()
	p.models = append(p.models, record.Model)
	p.mu.Unlock()
}

// Returning from Wait before a plugin completes would let shutdown close its
// durable sink while either the active event or a queued completion is pending.
func TestManagerWaitDrainsActiveAndQueuedPluginCompletions(t *testing.T) {
	manager := NewManager(4)
	plugin := &blockedUsagePlugin{entered: make(chan struct{}), release: make(chan struct{})}
	manager.Register(plugin)
	manager.Publish(context.Background(), Record{Model: "first"})
	<-plugin.entered
	manager.Publish(context.Background(), Record{Model: "second"})
	manager.Stop()
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := manager.Wait(cancelled); !errors.Is(err, context.Canceled) {
		t.Fatalf("unfinished plugin must remain pending until released: %v", err)
	}
	close(plugin.release)
	if err := manager.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	plugin.mu.Lock()
	defer plugin.mu.Unlock()
	if len(plugin.models) != 2 || plugin.models[0] != "first" || plugin.models[1] != "second" {
		t.Fatalf("Wait returned before accepted completions drained: %v", plugin.models)
	}
}

func TestManagerWaitAfterStopBeforeStart(t *testing.T) {
	manager := NewManager(4)
	manager.Stop()
	if err := manager.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
}
