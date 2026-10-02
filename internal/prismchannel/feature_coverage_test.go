package prismchannel

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"
)

// Feature-matrix tests: each TestPrismFeature* name maps 1:1 to a GOAL requirement.

func TestPrismFeatureHighConcurrency(t *testing.T) {
	m := NewMockUpstream()
	m.PollDelay = 5 * time.Millisecond
	c := testClient(t, m, func(cfg *Config) {
		cfg.AccountConcurrency = 8
		cfg.MaxWaiters = 64
		cfg.SandboxPoolSize = 2
		cfg.PrewarmSlots = 8
	})
	deadline := time.Now().Add(2 * time.Second)
	for c.metrics.WarmSlots.Load() < 8 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 24)
	for i := 0; i < 24; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := testRequest()
			r.Store = false
			_, e := c.Run(context.Background(), "conc-owner", r, nil)
			errs <- e
		}()
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		if e != nil {
			t.Fatal(e)
		}
	}
	if m.MaxActive.Load() > 8 || m.MaxActive.Load() < 2 || m.Violations.Load() != 0 || c.metrics.Inflight.Load() != 0 {
		t.Fatalf("concurrency envelope broken: max_active=%d violations=%d inflight=%d", m.MaxActive.Load(), m.Violations.Load(), c.metrics.Inflight.Load())
	}
	if c.Capacity() != 8 {
		t.Fatalf("capacity=%d want 8", c.Capacity())
	}
}

func TestPrismFeatureLowLatencyTTFPAndTPS(t *testing.T) {
	m := NewMockUpstream()
	m.PollCount = 3
	m.PollDelay = 2 * time.Millisecond
	m.Immediate = false
	c := testClient(t, m, func(cfg *Config) {
		cfg.PollMin = time.Millisecond
		cfg.PollMax = 2 * time.Millisecond
	})
	var firstDelta time.Duration
	started := time.Now()
	var sawText bool
	r, e := c.Run(context.Background(), "latency-owner", testRequest(), func(ev Event) error {
		if ev.Text != "" && !sawText {
			sawText = true
			firstDelta = time.Since(started)
		}
		return nil
	})
	if e != nil {
		t.Fatal(e)
	}
	if !sawText || firstDelta <= 0 || firstDelta > 250*time.Millisecond {
		t.Fatalf("mock TTFP not low-latency: saw=%v firstDelta=%s", sawText, firstDelta)
	}
	if c.metrics.TTFTCount.Load() != 1 || c.metrics.TTFTMicros.Load() == 0 {
		t.Fatal("TTFT metrics not recorded")
	}
	ttft := time.Duration(c.metrics.TTFTMicros.Load()) * time.Microsecond
	if ttft > 250*time.Millisecond {
		t.Fatalf("recorded TTFT too high under mock: %s", ttft)
	}
	if r.Usage.TotalTokens < 1 || c.metrics.CompletionTokens.Load() == 0 {
		t.Fatalf("TPS inputs missing: usage=%+v completion_tokens=%d", r.Usage, c.metrics.CompletionTokens.Load())
	}
	rpm, tpm := c.metrics.rates()
	if rpm < 1 || tpm < 1 {
		t.Fatalf("rolling TPS/RPM gauges empty: rpm=%d tpm=%d", rpm, tpm)
	}
	var metrics bytes.Buffer
	c.metrics.Write(&metrics, c.cache)
	for _, want := range []string{"prism_ttft_seconds_count 1", "prism_tpm ", "prism_rpm "} {
		if !strings.Contains(metrics.String(), want) {
			t.Fatalf("metric absent %q\n%s", want, metrics.String())
		}
	}
	t.Logf("mock TTFP=%s recorded_ttft=%s rpm=%d tpm=%d total_tokens=%d", firstDelta, ttft, rpm, tpm, r.Usage.TotalTokens)
}

func TestPrismFeatureToolCalling(t *testing.T) {
	m := NewMockUpstream()
	m.ToolName = "lookup"
	c := testClient(t, m, nil)
	r := testRequest()
	r.Tools = []Tool{{Type: "function", Name: "lookup", Parameters: json.RawMessage(`{"type":"object","properties":{"q":{"type":"string"}}}`)}}
	r.ToolChoice = "required"
	out, e := c.Run(context.Background(), "tool-owner", r, func(Event) error { return nil })
	if e != nil {
		t.Fatal(e)
	}
	if len(out.Calls) != 1 || out.Calls[0].Name != "lookup" || out.Calls[0].CallID == "" || out.Text != "" {
		t.Fatalf("tool call missing or text leaked: %+v", out)
	}
}

func TestPrismFeatureStreamingResponses(t *testing.T) {
	m := NewMockUpstream()
	m.PollCount = 5
	m.Text = "ABCDEFGHIJ"
	c := testClient(t, m, nil)
	var chunks []string
	out, e := c.Run(context.Background(), "stream-owner", testRequest(), func(ev Event) error {
		if ev.Text != "" {
			chunks = append(chunks, ev.Text)
		}
		return nil
	})
	if e != nil {
		t.Fatal(e)
	}
	joined := strings.Join(chunks, "")
	if joined != m.Text || out.Text != m.Text || len(chunks) < 2 {
		t.Fatalf("streaming deltas incomplete: chunks=%q out=%q", chunks, out.Text)
	}
}

func TestPrismFeatureImageVisionParsing(t *testing.T) {
	m := NewMockUpstream()
	c := testClient(t, m, func(cfg *Config) { cfg.ImageInline = true })
	r := testRequest()
	r.Input[0].Content = append(r.Input[0].Content, Content{Type: "input_image", ImageURL: pngData(t)})
	if _, e := c.Run(context.Background(), "vision-owner", r, nil); e != nil {
		t.Fatal(e)
	}
	if m.Uploads.Load() != 1 || c.metrics.Uploads.Load() != 1 {
		t.Fatalf("image upload not counted: mock=%d metrics=%d", m.Uploads.Load(), c.metrics.Uploads.Load())
	}
	inputs := m.Inputs()
	if len(inputs) == 0 {
		t.Fatal("no upstream input")
	}
	foundRef := false
	for _, it := range inputs[0] {
		for _, block := range it.Content {
			if block.Type == "input_image" && (strings.Contains(block.ImageURL, "/prism-uploads/") || block.Filename != "") {
				foundRef = true
			}
			if block.Type == "input_text" && strings.Contains(block.Text, "view_image") {
				foundRef = true
			}
		}
	}
	if !foundRef {
		t.Fatalf("vision parse did not emit upload ref or view_image: %+v", inputs[0])
	}
}

func TestPrismFeatureTokenCacheReadWriteCounting(t *testing.T) {
	m := NewMockUpstream()
	c := testClient(t, m, nil)
	first, e := c.Run(context.Background(), "cache-owner", testRequest(), nil)
	if e != nil {
		t.Fatal(e)
	}
	if first.CacheWriteTokens == 0 || c.metrics.CacheWrites.Load() != 1 || c.metrics.CacheWriteTokens.Load() == 0 || c.metrics.CacheWriteBytes.Load() == 0 {
		t.Fatalf("cache write counting missing: result=%+v writes=%d write_tokens=%d write_bytes=%d", first, c.metrics.CacheWrites.Load(), c.metrics.CacheWriteTokens.Load(), c.metrics.CacheWriteBytes.Load())
	}
	q := testRequest()
	q.PreviousID = first.ID
	q.Input = []Item{textItem("user", "followup")}
	second, e := c.Run(context.Background(), "cache-owner", q, nil)
	if e != nil {
		t.Fatal(e)
	}
	if second.CacheReadTokens == 0 || second.CacheReadTokens != first.CacheWriteTokens {
		t.Fatalf("cache read tokens mismatch: write=%d read=%d", first.CacheWriteTokens, second.CacheReadTokens)
	}
	if c.metrics.CacheHits.Load() != 1 || c.metrics.CacheReadTokens.Load() == 0 || c.metrics.CacheReadBytes.Load() == 0 {
		t.Fatal("cache read counters missing")
	}
	if second.CacheWriteTokens == 0 || c.metrics.CacheWrites.Load() != 2 {
		t.Fatalf("follow-up write counting missing: write=%d writes=%d", second.CacheWriteTokens, c.metrics.CacheWrites.Load())
	}
	var metrics bytes.Buffer
	c.metrics.Write(&metrics, c.cache)
	body := metrics.String()
	for _, want := range []string{"prism_cache_hits 1", "prism_cache_writes 2", "prism_cache_read_tokens ", "prism_cache_write_tokens "} {
		if !strings.Contains(body, want) {
			t.Fatalf("metric absent %q\n%s", want, body)
		}
	}
}
