// Package proxy is the gateway hot path: it authenticates-then-forwards client
// requests to OpenAI/Anthropic, passes provider responses through unmodified
// (never retrying), and observes token usage for metering. Streaming (SSE) is
// passed through with per-provider usage extractors; see extract.go.
package proxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/sh-rest/spendgate/internal/auth"
	"github.com/sh-rest/spendgate/internal/budget"
	"github.com/sh-rest/spendgate/internal/meter"
	"github.com/sh-rest/spendgate/internal/prices"
	"github.com/sh-rest/spendgate/internal/tenant"
)

// Provider describes one upstream API.
type Provider struct {
	Name    string // "openai" | "anthropic"
	Prefix  string // route prefix, e.g. "/openai"
	BaseURL string // e.g. https://api.openai.com
	APIKey  string // provider key attached to forwarded requests
}

// Proxy forwards to the configured providers and meters usage.
type Proxy struct {
	writer    *meter.Writer
	prices    prices.Table
	client    *http.Client
	budget    *budget.Client // nil disables budget enforcement entirely
	providers []Provider
}

// New builds a Proxy. A nil client uses one with no timeout (streaming-safe). A
// nil budget client disables enforcement (all requests forward unchecked).
func New(writer *meter.Writer, priceTable prices.Table, client *http.Client, bud *budget.Client, providers ...Provider) *Proxy {
	if client == nil {
		client = &http.Client{}
	}
	return &Proxy{writer: writer, prices: priceTable, client: client, budget: bud, providers: providers}
}

// reservation records a budget reservation made before forwarding, so the actual
// metered cost can be reconciled against the estimate once known. active is false
// when no reservation was made (no budget, no enforcement, or fail-open).
type reservation struct {
	active   bool
	key      string
	estMicro int64
}

func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	for _, pv := range p.providers {
		if strings.HasPrefix(r.URL.Path, pv.Prefix+"/") {
			p.forward(w, r, pv)
			return
		}
	}
	http.NotFound(w, r)
}

func (p *Proxy) forward(w http.ResponseWriter, r *http.Request, pv Provider) {
	start := time.Now()
	t, _ := auth.TenantFrom(r.Context()) // Middleware guarantees presence
	feature := r.Header.Get("X-Spendgate-Feature")

	body, _ := io.ReadAll(r.Body)
	_ = r.Body.Close()

	// ADR-003: ensure OpenAI streaming requests report usage.
	injected := false
	if pv.Name == "openai" {
		body, injected = maybeInjectUsage(body)
	}

	// Budget enforcement runs BEFORE forwarding, only for tenants with a cap set.
	res, forward := p.checkBudget(w, r, pv, t, body)
	if !forward {
		return // 429 (over budget) or 503 (Redis down, fail-closed) already written
	}

	target := pv.BaseURL + strings.TrimPrefix(r.URL.Path, pv.Prefix)
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}
	req, err := http.NewRequestWithContext(r.Context(), r.Method, target, bytes.NewReader(body))
	if err != nil {
		http.Error(w, "bad gateway", http.StatusBadGateway)
		p.enqueue(res, t, feature, pv.Name, requestModel(body), 0, 0, false, http.StatusBadGateway, start)
		return
	}
	copyHeaders(req.Header, r.Header)
	req.Header.Del("X-Spendgate-Feature") // internal attribution header, not forwarded
	req.Header.Del("Accept-Encoding")     // let the transport manage gzip so we see plaintext
	switch pv.Name {
	case "openai":
		req.Header.Set("Authorization", "Bearer "+pv.APIKey)
	case "anthropic":
		req.Header.Set("x-api-key", pv.APIKey)
	}
	req.ContentLength = int64(len(body))

	resp, err := p.client.Do(req)
	if err != nil {
		// Upstream unreachable. Never retry; surface as 502 and record it.
		http.Error(w, "upstream error", http.StatusBadGateway)
		p.enqueue(res, t, feature, pv.Name, requestModel(body), 0, 0, false, http.StatusBadGateway, start)
		return
	}
	defer resp.Body.Close()

	copyResponseHeaders(w.Header(), resp.Header)
	streaming := strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream")

	if streaming {
		p.streamResponse(w, r, resp, pv, injected, body, feature, t, res, start)
		return
	}
	p.bufferedResponse(w, resp, pv, body, feature, t, res, start)
}

// checkBudget reserves an estimated cost against the tenant's monthly cap before
// forwarding. Returns the reservation (to reconcile later) and whether to proceed.
// No cap set or no budget client → proceed unreserved. Over budget → 429. Redis
// unreachable → per-tenant fail_open: true forwards (unmetered-budget, warn),
// false returns 503.
func (p *Proxy) checkBudget(w http.ResponseWriter, r *http.Request, pv Provider, t tenant.Tenant, body []byte) (reservation, bool) {
	if t.MonthlyBudgetUSD == nil || p.budget == nil {
		return reservation{}, true
	}
	est := p.estimateMicro(pv, body)
	limit := toMicro(*t.MonthlyBudgetUSD)
	key := budget.MonthKey(t.ID, time.Now())

	allowed, spend, err := p.budget.Reserve(r.Context(), key, limit, est)
	if err != nil {
		if t.FailOpen {
			log.Printf("budget: redis unavailable for tenant %d, failing open (forwarding unmetered-budget): %v", t.ID, err)
			return reservation{}, true // still metered to Postgres downstream
		}
		writeBudgetUnavailable(w, t)
		return reservation{}, false
	}
	if !allowed {
		writeBudgetExceeded(w, t, *t.MonthlyBudgetUSD, float64(spend)/1e6)
		return reservation{}, false
	}
	return reservation{active: true, key: key, estMicro: est}, true
}

// estimateMicro is the conservative reservation cost in micro-USD. Input tokens
// are approximated as len(body)/4 (same chars/4 heuristic the metering fallback
// uses); output tokens use the request's max_tokens when set, else a constant
// ceiling. The real cost is reconciled afterwards, so the estimate only needs to
// be large enough to stop a request slipping past the cap, not exact.
// ponytail: unknown-priced models estimate to 0, so they reserve nothing and are
// effectively unenforced — you can't cap spend without a price. Add the model to
// prices.yaml to enforce it.
func (p *Proxy) estimateMicro(pv Provider, body []byte) int64 {
	in := len(body) / 4
	out := requestMaxTokens(body)
	if out <= 0 {
		out = defaultEstOutputTokens
	}
	cost, _ := p.prices.Cost(pv.Name, requestModel(body), in, out)
	return toMicro(cost)
}

const defaultEstOutputTokens = 1000

func toMicro(usd float64) int64 { return int64(math.Round(usd * 1e6)) }

// requestMaxTokens reads the output-token ceiling from a request body (OpenAI's
// max_tokens / max_completion_tokens, Anthropic's max_tokens), 0 if absent.
func requestMaxTokens(body []byte) int {
	var m struct {
		MaxTokens           int `json:"max_tokens"`
		MaxCompletionTokens int `json:"max_completion_tokens"`
	}
	_ = json.Unmarshal(body, &m)
	if m.MaxTokens > 0 {
		return m.MaxTokens
	}
	return m.MaxCompletionTokens
}

func writeBudgetExceeded(w http.ResponseWriter, t tenant.Tenant, budgetUSD, spendUSD float64) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusTooManyRequests)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{
			"type":       "budget_exceeded",
			"tenant":     t.Name,
			"budget_usd": budgetUSD,
			"spend_usd":  spendUSD,
		},
	})
}

func writeBudgetUnavailable(w http.ResponseWriter, t tenant.Tenant) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusServiceUnavailable)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{"type": "budget_unavailable", "tenant": t.Name},
	})
}

// bufferedResponse handles non-streaming responses: pass body through
// unmodified, parse usage on success, fall back to chars/4 when absent.
func (p *Proxy) bufferedResponse(w http.ResponseWriter, resp *http.Response, pv Provider, reqBody []byte, feature string, t tenant.Tenant, res reservation, start time.Time) {
	respBody, _ := io.ReadAll(resp.Body)
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(respBody)

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		p.enqueue(res, t, feature, pv.Name, requestModel(reqBody), 0, 0, false, resp.StatusCode, start)
		return
	}

	u := parseUsage(pv.Name, respBody)
	estimated := !u.HasUsage
	if estimated {
		u.InputTokens = len(reqBody) / 4  // chars/4 heuristic (DESIGN.md fallback)
		u.OutputTokens = len(respBody) / 4
	}
	p.enqueue(res, t, feature, pv.Name, modelOr(u.Model, reqBody), u.InputTokens, u.OutputTokens, estimated, resp.StatusCode, start)
}

// streamResponse passes an SSE stream through, flushing per event, while an
// extractor observes usage. A mid-stream disconnect still emits with whatever
// usage is known.
func (p *Proxy) streamResponse(w http.ResponseWriter, r *http.Request, resp *http.Response, pv Provider, injected bool, reqBody []byte, feature string, t tenant.Tenant, res reservation, start time.Time) {
	w.WriteHeader(resp.StatusCode)
	flush := func() {}
	if f, ok := w.(http.Flusher); ok {
		flush = f.Flush
	}

	ext := newExtractor(pv.Name)
	var drop func([][]byte) bool
	if pv.Name == "openai" && injected {
		drop = func(lines [][]byte) bool {
			for _, l := range lines {
				if isOpenAIUsageOnly(l) {
					return true
				}
			}
			return false
		}
	}

	// streamSSE returns non-nil on client disconnect / write error; we still meter.
	_ = streamSSE(w, flush, bufio.NewReader(resp.Body), ext, drop)

	u := ext.usage()
	p.enqueue(res, t, feature, pv.Name, modelOr(u.Model, reqBody), u.InputTokens, u.OutputTokens, !u.HasUsage, resp.StatusCode, start)
}

func (p *Proxy) enqueue(res reservation, t tenant.Tenant, feature, provider, model string, in, out int, estimated bool, status int, start time.Time) {
	cost, _ := p.prices.Cost(provider, model, in, out)
	p.reconcile(res, cost)
	p.writer.Enqueue(meter.Event{
		TenantID:     t.ID,
		Feature:      feature,
		Provider:     provider,
		Model:        model,
		InputTokens:  in,
		OutputTokens: out,
		CostUSD:      cost,
		Estimated:    estimated,
		Status:       status,
		LatencyMS:    int(time.Since(start).Milliseconds()),
		CreatedAt:    time.Now().UTC(),
	})
}

// reconcile adjusts the reserved estimate to the actual metered cost. On an error
// path (cost 0) this releases the whole reservation. Best-effort with a detached
// timeout: the client response is already sent, so a reconcile failure must not
// block, only log.
func (p *Proxy) reconcile(res reservation, cost float64) {
	if !res.active || p.budget == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := p.budget.Reconcile(ctx, res.key, toMicro(cost)-res.estMicro); err != nil {
		log.Printf("budget: reconcile %s: %v", res.key, err)
	}
}

func newExtractor(provider string) extractor {
	if provider == "anthropic" {
		return &anthropicExtractor{}
	}
	return &openaiExtractor{}
}

func parseUsage(provider string, body []byte) Usage {
	if provider == "anthropic" {
		return anthropicNonStreamUsage(body)
	}
	return openaiNonStreamUsage(body)
}

// maybeInjectUsage adds stream_options.include_usage to an OpenAI streaming
// request when the client omitted it (ADR-003). Returns the possibly-rewritten
// body and whether we injected. Non-streaming or client-set → unchanged.
func maybeInjectUsage(body []byte) ([]byte, bool) {
	var m map[string]json.RawMessage
	if json.Unmarshal(body, &m) != nil {
		return body, false
	}
	if s, ok := m["stream"]; !ok || !bytes.Equal(bytes.TrimSpace(s), []byte("true")) {
		return body, false
	}
	if so, ok := m["stream_options"]; ok {
		var opts map[string]json.RawMessage
		if json.Unmarshal(so, &opts) == nil {
			if _, has := opts["include_usage"]; has {
				return body, false // client set it — pass through untouched
			}
			opts["include_usage"] = json.RawMessage("true")
			nb, _ := json.Marshal(opts)
			m["stream_options"] = nb
			out, _ := json.Marshal(m)
			return out, true
		}
	}
	m["stream_options"] = json.RawMessage(`{"include_usage":true}`)
	out, _ := json.Marshal(m)
	return out, true
}

// requestModel extracts the "model" field from a request body, "" if absent.
func requestModel(body []byte) string {
	var m struct {
		Model string `json:"model"`
	}
	_ = json.Unmarshal(body, &m)
	return m.Model
}

func modelOr(model string, reqBody []byte) string {
	if model != "" {
		return model
	}
	return requestModel(reqBody)
}

// hopByHop headers are connection-specific and not forwarded.
var hopByHop = map[string]bool{
	"Connection": true, "Keep-Alive": true, "Proxy-Authenticate": true,
	"Proxy-Authorization": true, "Te": true, "Trailer": true,
	"Transfer-Encoding": true, "Upgrade": true,
}

func copyHeaders(dst, src http.Header) {
	for k, vs := range src {
		if hopByHop[http.CanonicalHeaderKey(k)] {
			continue
		}
		dst[k] = append([]string(nil), vs...)
	}
}

func copyResponseHeaders(dst, src http.Header) {
	for k, vs := range src {
		ck := http.CanonicalHeaderKey(k)
		// Transport-managed: our body may be decompressed / re-lengthed.
		if hopByHop[ck] || ck == "Content-Length" || ck == "Content-Encoding" {
			continue
		}
		dst[k] = append([]string(nil), vs...)
	}
}
