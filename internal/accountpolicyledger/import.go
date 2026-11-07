package accountpolicyledger

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	"io"
	"os"
	"time"
)

// ImportLegacy never modifies the original rotating files. A content checkpoint
// skips completed imports; synced UUID deduplication makes interruption safe.
func (a *Archive) ImportLegacy(dir string) error {
	if a == nil {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.legacyDir = dir
	if a.enabled == nil || !a.enabled() {
		return nil
	}
	return a.importLegacyLocked(dir)
}
func (a *Archive) prepare() error {
	if err := a.initialize(); err != nil {
		return err
	}
	if a.legacyDir != "" && !a.importComplete && a.enabled != nil && a.enabled() {
		return a.importLegacyLocked(a.legacyDir)
	}
	return nil
}
func (a *Archive) importLegacyLocked(dir string) error {
	if err := checkDirectory(dir); err != nil {
		return err
	}
	source, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer source.Close()
	if err = a.initialize(); err != nil {
		return err
	}
	if a.lookupMemo == nil {
		a.startLookupMemo()
		defer a.stopLookupMemo()
	}
	for _, name := range []string{"usage.2.jsonl", "usage.1.jsonl", "usage.jsonl"} {
		info, e := source.Lstat(name)
		if os.IsNotExist(e) {
			continue
		}
		if e != nil || !info.Mode().IsRegular() {
			return errors.New("unsafe legacy segment")
		}
		f, e := source.Open(name)
		if e != nil {
			return e
		}
		h := sha256.New()
		_, e = io.Copy(h, f)
		if e != nil {
			_ = f.Close()
			return e
		}
		digest := hex.EncodeToString(h.Sum(nil))
		marker := "import-" + digest + ".json"
		if _, e = a.root.Lstat(marker); e == nil {
			_ = f.Close()
			continue
		}
		if _, e = f.Seek(0, io.SeekStart); e != nil {
			_ = f.Close()
			return e
		}
		reader := bufio.NewReader(f)
		for {
			line, e := reader.ReadBytes('\n')
			if e == io.EOF {
				if len(line) > 0 {
					a.status.UncertainCoverage = append(a.status.UncertainCoverage, CoverageGap{Reason: "incomplete_legacy_tail"})
				}
				break
			}
			if e != nil {
				_ = f.Close()
				return e
			}
			var old struct {
				RequestID, Provider, CredentialID, Model                string
				RequestedAt                                             time.Time
				LatencyMS                                               float64
				Failed                                                  bool
				StatusCode                                              int
				InputTokens, OutputTokens, ReasoningTokens, TotalTokens int64
				CacheReadTokens, CacheWriteTokens                       *int64
			}
			// Legacy names are snake_case; use a raw allowlist to avoid ever carrying
			// unrecognized fields (including credential material) into the archive.
			var v map[string]json.RawMessage
			if json.Unmarshal(line, &v) != nil {
				a.status.Healthy = false
				a.status.UnhealthySegments = append(a.status.UnhealthySegments, "legacy:"+name)
				continue
			}
			fields := map[string]any{"request_id": &old.RequestID, "provider": &old.Provider, "credential_id": &old.CredentialID, "model": &old.Model, "requested_at": &old.RequestedAt, "latency_ms": &old.LatencyMS, "failed": &old.Failed, "status_code": &old.StatusCode, "input_tokens": &old.InputTokens, "output_tokens": &old.OutputTokens, "reasoning_tokens": &old.ReasoningTokens, "cache_read_tokens": &old.CacheReadTokens, "cache_write_tokens": &old.CacheWriteTokens, "total_tokens": &old.TotalTokens}
			for key, dst := range fields {
				if raw, ok := v[key]; ok {
					_ = json.Unmarshal(raw, dst)
				}
			}
			if old.RequestID == "" {
				a.status.Healthy = false
				continue
			}
			outcome := "success"
			if old.Failed {
				outcome = "failed"
			}
			detail := usage.Detail{InputTokens: old.InputTokens, OutputTokens: old.OutputTokens, ReasoningTokens: old.ReasoningTokens, TotalTokens: old.TotalTokens}
			if old.CacheReadTokens != nil {
				detail.CacheReadTokens = *old.CacheReadTokens
				detail.Measurements.CacheRead = true
			}
			if old.CacheWriteTokens != nil {
				detail.CacheCreationTokens = *old.CacheWriteTokens
				detail.Measurements.CacheWrite = true
			}
			detail = usage.EnsureTokenBreakdownForProvider(detail, old.Provider, "")
			item := Request{SchemaVersion: 1, RequestID: old.RequestID, Provider: old.Provider, CredentialID: old.CredentialID, RequestedModel: old.Model, RequestedAt: old.RequestedAt.UTC(), DurationMS: old.LatencyMS, Outcome: outcome, StatusCode: old.StatusCode, Tokens: detail.TokenBreakdown, Measurements: detail.Measurements, MeasurementQuality: "incomplete", Legacy: true}
			if e = a.append("requests", item.RequestID, item); e != nil {
				_ = f.Close()
				a.writeError()
				return e
			}
		}
		if e = f.Close(); e != nil {
			return e
		}
		if e = a.saveJSON(marker, map[string]any{"schema_version": 1, "source": name, "digest": digest}); e != nil {
			return e
		}
	}
	a.importComplete = true
	return nil
}
