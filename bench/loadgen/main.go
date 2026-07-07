// Command loadgen is a minimal closed-loop HTTP load generator (stdlib only,
// since neither vegeta nor k6 is installed). It runs -c workers hammering a
// URL for -d, each worker looping request-after-request (closed loop), fully
// draining every response body so streaming latency is measured end to end.
//
// It reports p50/p95/p99 latency and req/s. With -out it writes a single line
// "<p50> <p95> <p99> <rps> <count> <errors>" (ms) for the orchestration script.
package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"time"
)

func main() {
	url := flag.String("url", "", "target URL")
	conc := flag.Int("c", 500, "concurrent connections")
	dur := flag.Duration("d", 30*time.Second, "test duration")
	key := flag.String("key", "", "sg_ bearer key (empty = no auth header, for direct-to-provider)")
	stream := flag.Bool("stream", false, "send a streaming request body")
	warmup := flag.Duration("warmup", 2*time.Second, "warmup period (results discarded)")
	out := flag.String("out", "", "optional file to write result line to")
	flag.Parse()
	if *url == "" {
		fmt.Fprintln(os.Stderr, "loadgen: -url required")
		os.Exit(2)
	}

	body := []byte(`{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hello"}],"max_tokens":50}`)
	if *stream {
		body = []byte(`{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hello"}],"max_tokens":50,"stream":true}`)
	}

	// Transport sized so 500 workers each keep a warm keep-alive connection.
	tr := &http.Transport{MaxIdleConns: *conc * 2, MaxIdleConnsPerHost: *conc * 2, MaxConnsPerHost: 0, IdleConnTimeout: 90 * time.Second}
	client := &http.Client{Transport: tr, Timeout: 30 * time.Second}

	do := func() (time.Duration, bool) {
		start := time.Now()
		req, _ := http.NewRequest(http.MethodPost, *url, bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Spendgate-Feature", "bench")
		if *key != "" {
			req.Header.Set("Authorization", "Bearer "+*key)
		}
		resp, err := client.Do(req)
		if err != nil {
			return 0, false
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return 0, false
		}
		return time.Since(start), true
	}

	// Warmup: run workers briefly, discard measurements.
	runPhase(*conc, *warmup, do, false)

	// Measured phase.
	lats, errs := runPhase(*conc, *dur, do, true)

	sort.Slice(lats, func(i, j int) bool { return lats[i] < lats[j] })
	count := len(lats)
	p50 := pct(lats, 0.50)
	p95 := pct(lats, 0.95)
	p99 := pct(lats, 0.99)
	rps := float64(count) / dur.Seconds()

	fmt.Printf("count=%d errors=%d rps=%.0f p50=%.3fms p95=%.3fms p99=%.3fms\n",
		count, errs, rps, ms(p50), ms(p95), ms(p99))

	if *out != "" {
		line := fmt.Sprintf("%.3f %.3f %.3f %.0f %d %d\n", ms(p50), ms(p95), ms(p99), rps, count, errs)
		_ = os.WriteFile(*out, []byte(line), 0o644)
	}
}

// runPhase launches conc workers for d, each looping do() closed-loop. When
// record is true it returns all successful latencies (merged per-worker, no
// shared lock) and the error count.
func runPhase(conc int, d time.Duration, do func() (time.Duration, bool), record bool) ([]time.Duration, int) {
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()

	type result struct {
		lats []time.Duration
		errs int
	}
	results := make([]result, conc)
	done := make(chan int, conc)

	for i := 0; i < conc; i++ {
		go func(id int) {
			var r result
			for ctx.Err() == nil {
				lat, ok := do()
				if !record {
					continue
				}
				if ok {
					r.lats = append(r.lats, lat)
				} else {
					r.errs++
				}
			}
			results[id] = r
			done <- id
		}(i)
	}
	for i := 0; i < conc; i++ {
		<-done
	}

	var all []time.Duration
	errs := 0
	for _, r := range results {
		all = append(all, r.lats...)
		errs += r.errs
	}
	return all, errs
}

func pct(sorted []time.Duration, p float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	i := int(p * float64(len(sorted)))
	if i >= len(sorted) {
		i = len(sorted) - 1
	}
	return sorted[i]
}

func ms(d time.Duration) float64 { return float64(d.Microseconds()) / 1000.0 }
