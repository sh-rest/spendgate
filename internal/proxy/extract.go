package proxy

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
)

// Usage is the token accounting extracted from a provider response.
type Usage struct {
	InputTokens  int
	OutputTokens int
	Model        string
	HasUsage     bool // authoritative provider usage was seen
}

// extractor observes provider stream events and accumulates usage.
type extractor interface {
	observe(data []byte)
	usage() Usage
}

// --- OpenAI (stateless): usage arrives once, in a final choices:[] chunk. ---

type openaiExtractor struct{ u Usage }

func (e *openaiExtractor) usage() Usage { return e.u }

func (e *openaiExtractor) observe(data []byte) {
	// Never assume schema: unknown fields (e.g. "obfuscation") are ignored.
	var c struct {
		Model string `json:"model"`
		Usage *struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if json.Unmarshal(data, &c) != nil {
		return
	}
	if c.Model != "" {
		e.u.Model = c.Model
	}
	if c.Usage != nil {
		e.u.InputTokens = c.Usage.PromptTokens
		e.u.OutputTokens = c.Usage.CompletionTokens
		e.u.HasUsage = true
	}
}

// isOpenAIUsageOnly reports whether a chunk is the usage-only frame
// (choices:[] with non-null usage) that ADR-003 strips when we injected
// stream_options.include_usage ourselves.
func isOpenAIUsageOnly(data []byte) bool {
	var c struct {
		Choices []json.RawMessage `json:"choices"`
		Usage   json.RawMessage   `json:"usage"`
	}
	if json.Unmarshal(data, &c) != nil {
		return false
	}
	return len(c.Choices) == 0 && len(c.Usage) > 0 && !bytes.Equal(c.Usage, []byte("null"))
}

// --- Anthropic (state machine): input_tokens from message_start,
// authoritative output_tokens from message_delta. Tolerates ping + whitespace. ---

type anthropicExtractor struct{ u Usage }

func (e *anthropicExtractor) usage() Usage { return e.u }

func (e *anthropicExtractor) observe(data []byte) {
	var ev struct {
		Type    string `json:"type"`
		Message *struct {
			Model string `json:"model"`
			Usage *struct {
				InputTokens  int `json:"input_tokens"`
				OutputTokens int `json:"output_tokens"`
			} `json:"usage"`
		} `json:"message"`
		Usage *struct {
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	if json.Unmarshal(data, &ev) != nil {
		return
	}
	switch ev.Type {
	case "message_start":
		if ev.Message != nil {
			if ev.Message.Model != "" {
				e.u.Model = ev.Message.Model
			}
			if ev.Message.Usage != nil {
				e.u.InputTokens = ev.Message.Usage.InputTokens
				e.u.OutputTokens = ev.Message.Usage.OutputTokens
			}
		}
	case "message_delta":
		if ev.Usage != nil {
			e.u.OutputTokens = ev.Usage.OutputTokens
			e.u.HasUsage = true
		}
	}
}

// --- Non-streaming parsers ---

func openaiNonStreamUsage(body []byte) Usage {
	e := &openaiExtractor{}
	e.observe(body)
	return e.u
}

func anthropicNonStreamUsage(body []byte) Usage {
	var m struct {
		Model string `json:"model"`
		Usage *struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	var u Usage
	if json.Unmarshal(body, &m) != nil {
		return u
	}
	u.Model = m.Model
	if m.Usage != nil {
		u.InputTokens = m.Usage.InputTokens
		u.OutputTokens = m.Usage.OutputTokens
		u.HasUsage = true
	}
	return u
}

// streamSSE reads SSE events from src, feeds each `data:` payload to ext, and
// writes each event verbatim to dst (flushing after each) unless drop reports
// true for that event's data lines. Event framing is preserved byte-for-byte
// for forwarded events. Returns the first write error (client disconnect) or
// nil at end of stream.
func streamSSE(dst io.Writer, flush func(), src *bufio.Reader, ext extractor, drop func(dataLines [][]byte) bool) error {
	var raw bytes.Buffer
	var dataLines [][]byte

	flushEvent := func() error {
		if raw.Len() == 0 {
			return nil
		}
		for _, d := range dataLines {
			ext.observe(d)
		}
		forward := drop == nil || !drop(dataLines)
		defer func() { raw.Reset(); dataLines = nil }()
		if forward {
			if _, err := dst.Write(raw.Bytes()); err != nil {
				return err
			}
			flush()
		}
		return nil
	}

	for {
		line, err := src.ReadBytes('\n')
		if len(line) > 0 {
			raw.Write(line)
			trimmed := bytes.TrimRight(line, "\r\n")
			if len(bytes.TrimSpace(trimmed)) == 0 {
				if ferr := flushEvent(); ferr != nil {
					return ferr
				}
			} else if rest, ok := bytes.CutPrefix(trimmed, []byte("data:")); ok {
				dataLines = append(dataLines, bytes.TrimSpace(rest))
			}
		}
		if err != nil {
			ferr := flushEvent() // flush a trailing event with no blank line
			if err == io.EOF {
				return ferr
			}
			return err
		}
	}
}
