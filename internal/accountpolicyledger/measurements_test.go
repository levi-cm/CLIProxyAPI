package accountpolicyledger

import (
	"context"
	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	"testing"
	"time"
)

func TestWeightedCacheMeasurementCoverageAndModels(t *testing.T) {
	a, _ := New(t.TempDir(), func() bool { return true })
	defer a.Close()
	for i, d := range []usage.Detail{
		{InputTokens: 1000, CacheReadTokens: 800, TotalTokens: 1000, Measurements: usage.MeasurementPresence{Input: true, CacheRead: true}},
		{InputTokens: 9000, CacheCreationTokens: 400, TotalTokens: 9000, Measurements: usage.MeasurementPresence{Input: true, CacheRead: true, CacheWrite: true}},
		{InputTokens: 5000, TotalTokens: 5000},
	} {
		a.HandleUsage(context.Background(), usage.Record{RequestID: uuid.NewString(), Provider: "codex", Model: "executed", Alias: "requested", ResponseModel: "served", RequestedAt: time.Date(2026, 1, 1, 0, i, 0, 0, time.UTC), Detail: d})
	}
	p, err := a.Requests(Query{})
	if err != nil {
		t.Fatal(err)
	}
	if p.Records[0].RequestedModel != "requested" || p.Records[0].ExecutionModel != "executed" || p.Records[0].ResponseModel != "served" {
		t.Fatal("lost model identities")
	}
	if p.Records[1].CachedInputPercent == nil || *p.Records[1].CachedInputPercent != 0 || p.Records[2].CachedInputPercent != nil {
		t.Fatal("zero/absent measurement confused")
	}
	sums, err := a.Summary(Query{}, "day")
	if err != nil {
		t.Fatal(err)
	}
	if len(sums) != 1 || sums[0].CachedInputPercent == nil || *sums[0].CachedInputPercent != 8 || sums[0].MeasuredAttempts != 2 {
		t.Fatalf("weighted coverage %+v", sums)
	}
}
