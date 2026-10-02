package prismchannel

import (
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"
)

type metricBucket struct {
	second           int64
	requests, tokens uint64
}
type Metrics struct {
	BackendNewErrors, BackendNew429, StartErrors, Start429                              atomic.Uint64
	LiveVerifiedAt                                                                      atomic.Int64
	UploadStorageReferences, UploadURLs, ContinuitySnapshots, ContinuitySnapshotMissing atomic.Uint64
	Requests, Completed, Errors, Rejected, Polls, Stops, StopErrors                     atomic.Uint64
	PromptTokens, CompletionTokens, EstimatedUsage                                      atomic.Uint64
	CacheHits, CacheMisses, CacheWrites, CacheReadTokens, CacheWriteTokens              atomic.Uint64
	CacheReadBytes, CacheWriteBytes, CacheEvictions, CacheSkipped                       atomic.Uint64
	Uploads, Warmups, WarmupErrors, TTFTCount, TTFTMicros                               atomic.Uint64
	PacedWaiters, Inflight, Queued, WarmSlots                                           atomic.Int64
	mu                                                                                  sync.Mutex
	buckets                                                                             [60]metricBucket
}

func (m *Metrics) meter(requests, tokens uint64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	sec := time.Now().Unix()
	b := &m.buckets[sec%60]
	if b.second != sec {
		*b = metricBucket{second: sec}
	}
	b.requests += requests
	b.tokens += tokens
}
func (m *Metrics) rates() (uint64, uint64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now().Unix()
	var r, t uint64
	for _, b := range m.buckets {
		if b.second > now-60 {
			r += b.requests
			t += b.tokens
		}
	}
	return r, t
}
func (m *Metrics) Write(w io.Writer, cache *TokenCache) {
	for _, v := range []struct {
		name  string
		value uint64
	}{
		{"requests", m.Requests.Load()}, {"completed", m.Completed.Load()}, {"errors", m.Errors.Load()}, {"rejected", m.Rejected.Load()}, {"polls", m.Polls.Load()}, {"stops", m.Stops.Load()}, {"stop_errors", m.StopErrors.Load()},
		{"prompt_tokens", m.PromptTokens.Load()}, {"completion_tokens", m.CompletionTokens.Load()}, {"estimated_usage", m.EstimatedUsage.Load()},
		{"cache_hits", m.CacheHits.Load()}, {"cache_misses", m.CacheMisses.Load()}, {"cache_writes", m.CacheWrites.Load()}, {"cache_read_tokens", m.CacheReadTokens.Load()}, {"cache_write_tokens", m.CacheWriteTokens.Load()},
		{"cache_read_bytes", m.CacheReadBytes.Load()}, {"cache_write_bytes", m.CacheWriteBytes.Load()}, {"cache_evictions", m.CacheEvictions.Load()}, {"cache_skipped", m.CacheSkipped.Load()},
		{"backend_new_errors", m.BackendNewErrors.Load()}, {"backend_new_429", m.BackendNew429.Load()}, {"start_errors", m.StartErrors.Load()}, {"start_429", m.Start429.Load()}, {"continuity_snapshots", m.ContinuitySnapshots.Load()}, {"continuity_snapshot_missing", m.ContinuitySnapshotMissing.Load()}, {"upload_storage_references", m.UploadStorageReferences.Load()}, {"upload_urls", m.UploadURLs.Load()}, {"uploads", m.Uploads.Load()}, {"warmups", m.Warmups.Load()}, {"warmup_errors", m.WarmupErrors.Load()},
	} {
		fmt.Fprintf(w, "# TYPE prism_%s counter\nprism_%s %d\n", v.name, v.name, v.value)
	}
	n, b := cache.Size()
	rpm, tpm := m.rates()
	for _, v := range []struct {
		name  string
		value int64
	}{{"paced_waiters", m.PacedWaiters.Load()}, {"inflight", m.Inflight.Load()}, {"queued", m.Queued.Load()}, {"warm_slots", m.WarmSlots.Load()}, {"cache_entries", int64(n)}, {"cache_bytes", b}, {"rpm", int64(rpm)}, {"tpm", int64(tpm)}} {
		fmt.Fprintf(w, "# TYPE prism_%s gauge\nprism_%s %d\n", v.name, v.name, v.value)
	}
	fmt.Fprintf(w, "# TYPE prism_ttft_seconds summary\nprism_ttft_seconds_count %d\nprism_ttft_seconds_sum %.6f\n", m.TTFTCount.Load(), float64(m.TTFTMicros.Load())/1e6)
}
