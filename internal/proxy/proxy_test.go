package proxy

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sh-rest/spendgate/internal/auth"
	"github.com/sh-rest/spendgate/internal/budget"
	"github.com/sh-rest/spendgate/internal/meter"
	"github.com/sh-rest/spendgate/internal/prices"
	"github.com/sh-rest/spendgate/internal/tenant"
)

// captureSink records metering events for assertions.
type captureSink struct {
	mu     sync.Mutex
	events []meter.Event
}

func (s *captureSink) Write(_ context.Context, e []meter.Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, e...)
	return nil
}

func (s *captureSink) snapshot() []meter.Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]meter.Event(nil), s.events...)
}

// testHarness wires a Proxy (behind auth) to a fake provider and a capture sink.
type testHarness struct {
	handler  http.Handler
	sink     *captureSink
	stop     func()
	lastReq  *http.Request // last request the fake provider saw
	lastBody string
	budget   *budget.Client // nil = enforcement disabled
	tnt      tenant.Tenant  // tenant the auth stub returns
}

func newHarness(t *testing.T, providerName string, fake http.HandlerFunc) *testHarness {
	return newBudgetHarness(t, providerName, fake, nil, tenant.Tenant{ID: 7})
}

func newBudgetHarness(t *testing.T, providerName string, fake http.HandlerFunc, bud *budget.Client, tnt tenant.Tenant) *testHarness {
	t.Helper()
	upstream := httptest.NewServer(nil)
	h := &testHarness{sink: &captureSink{}, budget: bud, tnt: tnt}
	upstream.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := readAll(r)
		h.lastReq = r
		h.lastBody = body
		r.Body = io.NopCloser(strings.NewReader(body)) // restore for fake to re-read
		fake(w, r)
	})

	writer := meter.New(h.sink, 1, time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	go writer.Run(ctx)

	table := prices.BuildTable([]prices.Price{
		{Provider: "openai", Model: "gpt-4o-mini-2024-07-18", InputUSDPerMTok: 0.15, OutputUSDPerMTok: 0.60},
		{Provider: "anthropic", Model: "claude-haiku-4-5-20251001", InputUSDPerMTok: 1.0, OutputUSDPerMTok: 5.0},
	})
	p := New(writer, table, nil, h.budget, Provider{
		Name: providerName, Prefix: "/" + providerName, BaseURL: upstream.URL, APIKey: "provider-secret",
	})
	authr := auth.New(func(context.Context, string) (tenant.Tenant, bool, error) {
		return h.tnt, true, nil
	}, time.Minute)

	h.handler = authr.Middleware(p)
	h.stop = func() {
		cancel()
		writer.Wait()
		upstream.Close()
	}
	return h
}

func readAll(r *http.Request) (string, error) {
	var b bytes.Buffer
	_, err := b.ReadFrom(r.Body)
	return b.String(), err
}

func newProxyRequest(provider, body string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/"+provider+"/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer sg_clientkey")
	req.Header.Set("X-Spendgate-Feature", "search")
	return req
}

// sseDataLines returns the ordered data: payloads of an SSE stream, ignoring
// event framing — the basis for chunk-boundary-tolerant content comparison.
func sseDataLines(s string) []string {
	var out []string
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimRight(line, "\r")
		if rest, ok := strings.CutPrefix(line, "data:"); ok {
			out = append(out, strings.TrimSpace(rest))
		}
	}
	return out
}

// fakeOpenAIStream serves the with-usage fixture when the request asked for
// usage, else the no-usage fixture — mirroring real OpenAI behaviour.
func fakeOpenAIStream(t *testing.T) http.HandlerFunc {
	with := readFixture(t, "openai-stream-with-usage.sse")
	without := readFixture(t, "openai-stream-no-usage.sse")
	return func(w http.ResponseWriter, r *http.Request) {
		body, _ := readAll(r)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if strings.Contains(body, `"include_usage":true`) {
			_, _ = w.Write(with)
		} else {
			_, _ = w.Write(without)
		}
	}
}

// TestADR003InjectAndStrip: client omits stream_options → proxy injects
// include_usage, extracts usage, and strips the usage-only chunk so the client
// sees a stream identical to the no-usage response.
func TestADR003InjectAndStrip(t *testing.T) {
	h := newHarness(t, "openai", fakeOpenAIStream(t))
	defer h.stop()

	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, newProxyRequest("openai", `{"model":"gpt-4o-mini","stream":true}`))

	// Provider must have received the injected option and the provider key, not
	// the client key.
	if !strings.Contains(h.lastBody, `"include_usage":true`) {
		t.Errorf("provider did not receive injected include_usage: %s", h.lastBody)
	}
	if got := h.lastReq.Header.Get("Authorization"); got != "Bearer provider-secret" {
		t.Errorf("provider Authorization = %q, want provider key", got)
	}

	// Client-facing stream must be the provider stream with the usage-only frame
	// removed: same content chunks + [DONE], no choices:[]+usage frame.
	got := sseDataLines(rec.Body.String())
	var want []string
	for _, d := range sseDataLines(string(readFixture(t, "openai-stream-with-usage.sse"))) {
		if d != "[DONE]" && isOpenAIUsageOnly([]byte(d)) {
			continue // this is the frame we expect stripped
		}
		want = append(want, d)
	}
	if !equalStrings(got, want) {
		t.Errorf("client stream not stripped correctly:\n got=%v\nwant=%v", got, want)
	}
	for _, d := range got {
		if d != "[DONE]" && isOpenAIUsageOnly([]byte(d)) {
			t.Errorf("usage-only frame leaked to client: %s", d)
		}
	}

	// Usage still extracted and metered.
	ev := h.oneEvent(t)
	if ev.InputTokens != 9 || ev.OutputTokens != 10 || ev.Estimated {
		t.Errorf("metered usage = %+v, want in=9 out=10 estimated=false", ev)
	}
	if ev.Feature != "search" || ev.TenantID != 7 {
		t.Errorf("attribution wrong: feature=%q tenant=%d", ev.Feature, ev.TenantID)
	}
}

// TestStreamPassthroughIdentity: client sets include_usage itself → no
// injection, no stripping; the client-facing stream is content-identical to the
// provider stream (chunk-boundary tolerant).
func TestStreamPassthroughIdentity(t *testing.T) {
	h := newHarness(t, "openai", fakeOpenAIStream(t))
	defer h.stop()

	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, newProxyRequest("openai",
		`{"model":"gpt-4o-mini","stream":true,"stream_options":{"include_usage":true}}`))

	got := sseDataLines(rec.Body.String())
	want := sseDataLines(string(readFixture(t, "openai-stream-with-usage.sse")))
	if !equalStrings(got, want) {
		t.Errorf("passthrough not content-identical:\n got=%v\nwant=%v", got, want)
	}
	if ev := h.oneEvent(t); ev.OutputTokens != 10 {
		t.Errorf("usage out=%d, want 10", ev.OutputTokens)
	}
}

func TestAnthropicStreamPassthrough(t *testing.T) {
	fixture := readFixture(t, "anthropic-stream.sse")
	h := newHarness(t, "anthropic", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(fixture)
	})
	defer h.stop()

	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, newProxyRequest("anthropic", `{"model":"claude","stream":true}`))

	if got, want := sseDataLines(rec.Body.String()), sseDataLines(string(fixture)); !equalStrings(got, want) {
		t.Errorf("anthropic passthrough altered stream")
	}
	if got := h.lastReq.Header.Get("X-Api-Key"); got != "provider-secret" {
		t.Errorf("anthropic x-api-key = %q, want provider key", got)
	}
	ev := h.oneEvent(t)
	if ev.InputTokens != 9 || ev.OutputTokens != 14 {
		t.Errorf("anthropic usage = in%d/out%d, want 9/14", ev.InputTokens, ev.OutputTokens)
	}
}

func TestNonStreamMetering(t *testing.T) {
	body := readFixture(t, "openai-nonstream.json")
	h := newHarness(t, "openai", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	})
	defer h.stop()

	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, newProxyRequest("openai", `{"model":"gpt-4o-mini"}`))

	if rec.Body.String() != string(body) {
		t.Errorf("non-stream body not passed through unmodified")
	}
	ev := h.oneEvent(t)
	if ev.InputTokens != 9 || ev.OutputTokens != 10 || ev.Estimated {
		t.Errorf("usage = %+v, want 9/10 not estimated", ev)
	}
	// cost = 9/1e6*0.15 + 10/1e6*0.60
	if want := 9.0/1e6*0.15 + 10.0/1e6*0.60; ev.CostUSD != want {
		t.Errorf("cost = %v, want %v", ev.CostUSD, want)
	}
}

// TestProviderErrorPassthrough: provider 401 passes through byte-for-byte, is
// metered with status, and is not marked estimated.
func TestProviderErrorPassthrough(t *testing.T) {
	body := readFixture(t, "openai-error.json")
	h := newHarness(t, "openai", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write(body)
	})
	defer h.stop()

	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, newProxyRequest("openai", `{"model":"gpt-4o-mini"}`))

	if rec.Code != http.StatusUnauthorized || rec.Body.String() != string(body) {
		t.Errorf("provider error not passed through: code=%d", rec.Code)
	}
	ev := h.oneEvent(t)
	if ev.Status != http.StatusUnauthorized || ev.Estimated || ev.InputTokens != 0 {
		t.Errorf("error event = %+v, want status 401, not estimated, 0 tokens", ev)
	}
}

// failWriter fails Write after n successful writes, simulating a mid-stream
// client disconnect.
type failWriter struct {
	h       http.Header
	wrote   int
	failAt  int
	code    int
	flushed int
}

func (f *failWriter) Header() http.Header { return f.h }
func (f *failWriter) WriteHeader(c int)   { f.code = c }
func (f *failWriter) Flush()              { f.flushed++ }
func (f *failWriter) Write(b []byte) (int, error) {
	f.wrote++
	if f.wrote > f.failAt {
		return 0, errors.New("client gone")
	}
	return len(b), nil
}

func TestMidStreamDisconnectStillMeters(t *testing.T) {
	fixture := readFixture(t, "anthropic-stream.sse")
	h := newHarness(t, "anthropic", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(fixture)
	})
	defer h.stop()

	fw := &failWriter{h: http.Header{}, failAt: 2} // fail on the 3rd event write
	h.handler.ServeHTTP(fw, newProxyRequest("anthropic", `{"model":"claude","stream":true}`))

	ev := h.oneEvent(t)
	// message_start was observed before the disconnect: input tokens known.
	if ev.InputTokens != 9 || ev.Status != http.StatusOK {
		t.Errorf("disconnect event = %+v, want input=9 status=200", ev)
	}
}

func (h *testHarness) oneEvent(t *testing.T) meter.Event {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	var evs []meter.Event
	for time.Now().Before(deadline) {
		if evs = h.sink.snapshot(); len(evs) > 0 {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	if len(evs) != 1 {
		t.Fatalf("want exactly 1 metering event, got %d", len(evs))
	}
	return evs[0]
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
