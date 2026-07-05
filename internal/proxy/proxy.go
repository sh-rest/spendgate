// Package proxy is the gateway hot path: it authenticates-then-forwards client
// requests to OpenAI/Anthropic, passes provider responses through unmodified
// (never retrying), and observes token usage for metering. Streaming (SSE) is
// passed through with per-provider usage extractors; see extract.go.
package proxy

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/sh-rest/spendgate/internal/auth"
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
	providers []Provider
}

// New builds a Proxy. A nil client uses one with no timeout (streaming-safe).
func New(writer *meter.Writer, priceTable prices.Table, client *http.Client, providers ...Provider) *Proxy {
	if client == nil {
		client = &http.Client{}
	}
	return &Proxy{writer: writer, prices: priceTable, client: client, providers: providers}
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

	target := pv.BaseURL + strings.TrimPrefix(r.URL.Path, pv.Prefix)
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}
	req, err := http.NewRequestWithContext(r.Context(), r.Method, target, bytes.NewReader(body))
	if err != nil {
		http.Error(w, "bad gateway", http.StatusBadGateway)
		p.enqueue(t, feature, pv.Name, requestModel(body), 0, 0, false, http.StatusBadGateway, start)
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
		p.enqueue(t, feature, pv.Name, requestModel(body), 0, 0, false, http.StatusBadGateway, start)
		return
	}
	defer resp.Body.Close()

	copyResponseHeaders(w.Header(), resp.Header)
	streaming := strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream")

	if streaming {
		p.streamResponse(w, r, resp, pv, injected, body, feature, t, start)
		return
	}
	p.bufferedResponse(w, resp, pv, body, feature, t, start)
}

// bufferedResponse handles non-streaming responses: pass body through
// unmodified, parse usage on success, fall back to chars/4 when absent.
func (p *Proxy) bufferedResponse(w http.ResponseWriter, resp *http.Response, pv Provider, reqBody []byte, feature string, t tenant.Tenant, start time.Time) {
	respBody, _ := io.ReadAll(resp.Body)
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(respBody)

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		p.enqueue(t, feature, pv.Name, requestModel(reqBody), 0, 0, false, resp.StatusCode, start)
		return
	}

	u := parseUsage(pv.Name, respBody)
	estimated := !u.HasUsage
	if estimated {
		u.InputTokens = len(reqBody) / 4  // chars/4 heuristic (DESIGN.md fallback)
		u.OutputTokens = len(respBody) / 4
	}
	p.enqueue(t, feature, pv.Name, modelOr(u.Model, reqBody), u.InputTokens, u.OutputTokens, estimated, resp.StatusCode, start)
}

// streamResponse passes an SSE stream through, flushing per event, while an
// extractor observes usage. A mid-stream disconnect still emits with whatever
// usage is known.
func (p *Proxy) streamResponse(w http.ResponseWriter, r *http.Request, resp *http.Response, pv Provider, injected bool, reqBody []byte, feature string, t tenant.Tenant, start time.Time) {
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
	p.enqueue(t, feature, pv.Name, modelOr(u.Model, reqBody), u.InputTokens, u.OutputTokens, !u.HasUsage, resp.StatusCode, start)
}

func (p *Proxy) enqueue(t tenant.Tenant, feature, provider, model string, in, out int, estimated bool, status int, start time.Time) {
	cost, _ := p.prices.Cost(provider, model, in, out)
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
