package proxy

import (
	"bufio"
	"bytes"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return b
}

// runStream feeds an SSE fixture through streamSSE (discarding output) and
// returns the extracted usage — exercising both the parser and the extractor.
func runStream(t *testing.T, fixture string, ext extractor) Usage {
	t.Helper()
	src := bufio.NewReader(bytes.NewReader(readFixture(t, fixture)))
	if err := streamSSE(io.Discard, func() {}, src, ext, nil); err != nil {
		t.Fatalf("streamSSE: %v", err)
	}
	return ext.usage()
}

func TestStreamExtractors(t *testing.T) {
	tests := []struct {
		name    string
		fixture string
		ext     extractor
		want    Usage
	}{
		{
			name:    "openai stream with usage",
			fixture: "openai-stream-with-usage.sse",
			ext:     &openaiExtractor{},
			want:    Usage{InputTokens: 9, OutputTokens: 10, Model: "gpt-4o-mini-2024-07-18", HasUsage: true},
		},
		{
			name:    "openai stream without usage",
			fixture: "openai-stream-no-usage.sse",
			ext:     &openaiExtractor{},
			want:    Usage{InputTokens: 0, OutputTokens: 0, Model: "gpt-4o-mini-2024-07-18", HasUsage: false},
		},
		{
			// output_tokens is authoritative from message_delta (14), not the
			// provisional 2 in message_start; ping + trailing whitespace tolerated.
			name:    "anthropic stream state machine",
			fixture: "anthropic-stream.sse",
			ext:     &anthropicExtractor{},
			want:    Usage{InputTokens: 9, OutputTokens: 14, Model: "claude-haiku-4-5-20251001", HasUsage: true},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := runStream(t, tc.fixture, tc.ext); got != tc.want {
				t.Errorf("usage = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestNonStreamExtractors(t *testing.T) {
	tests := []struct {
		name    string
		fixture string
		parse   func([]byte) Usage
		want    Usage
	}{
		{
			name:    "openai non-stream",
			fixture: "openai-nonstream.json",
			parse:   openaiNonStreamUsage,
			want:    Usage{InputTokens: 9, OutputTokens: 10, Model: "gpt-4o-mini-2024-07-18", HasUsage: true},
		},
		{
			name:    "anthropic non-stream",
			fixture: "anthropic-nonstream.json",
			parse:   anthropicNonStreamUsage,
			want:    Usage{InputTokens: 9, OutputTokens: 16, Model: "claude-haiku-4-5-20251001", HasUsage: true},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.parse(readFixture(t, tc.fixture)); got != tc.want {
				t.Errorf("usage = %+v, want %+v", got, tc.want)
			}
		})
	}
}
