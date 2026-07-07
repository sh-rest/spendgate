// Command fakeprovider mimics the OpenAI chat completions endpoint with
// configurable latency, returning a valid usage block so spendgate's metering
// path runs for real. Serves the non-streaming JSON response and, when the
// request body has "stream":true, an SSE stream ending in a usage frame.
//
// It listens on /v1/chat/completions (spendgate strips its /openai prefix, so
// forwarded requests land here). Latency (-latency) simulates provider compute.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"time"
)

func main() {
	addr := flag.String("addr", ":9090", "listen address")
	latency := flag.Duration("latency", 10*time.Millisecond, "simulated provider latency before responding")
	promptTokens := flag.Int("prompt-tokens", 12, "usage.prompt_tokens reported in every response")
	completionTokens := flag.Int("completion-tokens", 8, "usage.completion_tokens reported in every response")
	flag.Parse()

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = r.Body.Close()
		var req struct {
			Model  string `json:"model"`
			Stream bool   `json:"stream"`
		}
		_ = json.Unmarshal(body, &req)
		if req.Model == "" {
			req.Model = "gpt-4o-mini"
		}

		time.Sleep(*latency) // ponytail: fixed latency, add jitter only if a scenario needs it

		if req.Stream {
			streamResponse(w, req.Model, *promptTokens, *completionTokens)
			return
		}
		nonStream(w, req.Model, *promptTokens, *completionTokens)
	})

	log.Printf("fakeprovider listening on %s (latency %s)", *addr, *latency)
	log.Fatal(http.ListenAndServe(*addr, mux))
}

func nonStream(w http.ResponseWriter, model string, promptTokens, completionTokens int) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"id":      "chatcmpl-bench",
		"object":  "chat.completion",
		"model":   model,
		"choices": []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": "hello from fakeprovider"}, "finish_reason": "stop"}},
		"usage":   map[string]any{"prompt_tokens": promptTokens, "completion_tokens": completionTokens, "total_tokens": promptTokens + completionTokens},
	})
}

// streamResponse emits a few content deltas then a usage-bearing final frame
// (choices:[] with usage), matching OpenAI include_usage streaming shape.
func streamResponse(w http.ResponseWriter, model string, promptTokens, completionTokens int) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	f, _ := w.(http.Flusher)

	delta := func(content string) {
		fmt.Fprintf(w, "data: {\"id\":\"chatcmpl-bench\",\"object\":\"chat.completion.chunk\",\"model\":%q,\"choices\":[{\"index\":0,\"delta\":{\"content\":%q}}]}\n\n", model, content)
		if f != nil {
			f.Flush()
		}
	}
	for _, tok := range []string{"hello", " from", " fake", " provider"} {
		delta(tok)
	}
	// Final usage-only frame.
	fmt.Fprintf(w, "data: {\"id\":\"chatcmpl-bench\",\"object\":\"chat.completion.chunk\",\"model\":%q,\"choices\":[],\"usage\":{\"prompt_tokens\":%d,\"completion_tokens\":%d,\"total_tokens\":%d}}\n\n", model, promptTokens, completionTokens, promptTokens+completionTokens)
	fmt.Fprint(w, "data: [DONE]\n\n")
	if f != nil {
		f.Flush()
	}
}
