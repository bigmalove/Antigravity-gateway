package native

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"antigravity-gateway/internal/config"
	"antigravity-gateway/internal/synthetic"
)

// StreamTransformer converts a native streamGenerateContent SSE response into
// OpenAI-compatible chat.completion.chunk SSE. Incremental functionCall
// partialArgs for the single owned "content" string are fed, escaped form
// verbatim, into the existing incremental JSON string decoder, so each segment
// is emitted as delta.content as soon as the upstream produces it.
type StreamTransformer struct {
	cfg      *config.Config
	toolName string
	model    string
}

func NewStreamTransformer(cfg *config.Config, toolName, model string) *StreamTransformer {
	return &StreamTransformer{cfg: cfg, toolName: toolName, model: model}
}

type nativeChunk struct {
	Candidates     []nativeCandidate `json:"candidates"`
	UsageMetadata  json.RawMessage   `json:"usageMetadata"`
	PromptFeedback struct {
		BlockReason string `json:"blockReason"`
	} `json:"promptFeedback"`
	Error *struct {
		Message string `json:"message"`
		Status  string `json:"status"`
	} `json:"error"`
}

type nativeCandidate struct {
	Content      nativeContent `json:"content"`
	FinishReason string        `json:"finishReason"`
}

type nativeContent struct {
	Parts []nativePart `json:"parts"`
}

type nativePart struct {
	Text         *string             `json:"text"`
	Thought      bool                `json:"thought"`
	FunctionCall *nativeFunctionCall `json:"functionCall"`
}

type nativeFunctionCall struct {
	Name         string             `json:"name"`
	Args         json.RawMessage    `json:"args"`
	PartialArgs  []nativePartialArg `json:"partialArgs"`
	WillContinue *bool              `json:"willContinue"`
}

type nativePartialArg struct {
	JSONPath     string          `json:"jsonPath"`
	StringValue  json.RawMessage `json:"stringValue"`
	WillContinue *bool           `json:"willContinue"`
}

func randomHex(nBytes int) string {
	b := make([]byte, nBytes)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%016x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

func rawJSONStringInner(raw json.RawMessage) (string, bool) {
	if len(raw) < 2 || raw[0] != '"' || raw[len(raw)-1] != '"' {
		return "", false
	}
	return string(raw[1 : len(raw)-1]), true
}

// Transform reads native SSE from upstream and writes OpenAI SSE to w.
// It returns a protocol error (and stops without emitting [DONE]) when the
// native stream violates the wrapper invariants: a clean terminal state is the
// only successful end, so a truncated or interrupted stream can never be
// presented as a completed answer.
func (t *StreamTransformer) Transform(upstream io.Reader, w io.Writer, flusher http.Flusher) (*synthetic.StreamStats, error) {
	stats := &synthetic.StreamStats{}
	scanner := bufio.NewScanner(upstream)
	scanner.Buffer(make([]byte, 64*1024), int(t.cfg.MaxResponseBytes))

	id := "chatcmpl-" + randomHex(16)
	created := time.Now().Unix()
	decoder := synthetic.NewIncrementalJSONStringDecoder()

	var (
		started       bool
		stringStarted bool
		stringClosed  bool
		callClosed    bool
		fullArgs      bool
		terminal      bool
		usage         map[string]any
	)

	writeSSEData := func(data any) error {
		b, err := json.Marshal(data)
		if err != nil {
			return err
		}
		n, err := io.WriteString(w, "data: "+string(b)+"\n\n")
		stats.BytesWritten += int64(n)
		if flusher != nil {
			flusher.Flush()
		}
		return err
	}
	emitDelta := func(fields map[string]any, finishReason any) error {
		return writeSSEData(map[string]any{
			"id":      id,
			"object":  "chat.completion.chunk",
			"created": created,
			"model":   t.model,
			"choices": []map[string]any{
				{"index": 0, "delta": fields, "finish_reason": finishReason},
			},
		})
	}
	emitContent := func(text string) error {
		if text == "" {
			return nil
		}
		return emitDelta(map[string]any{"content": text}, nil)
	}
	feed := func(fragment string) error {
		out, _, err := decoder.Feed(fragment)
		if err != nil {
			return err
		}
		return emitContent(out)
	}
	finish := func(reason string) error {
		if terminal {
			return ProtocolError("duplicate_finish")
		}
		if reason == "STOP" && started && !callClosed {
			// A clean stop cannot leave the transport call half-open.
			return ProtocolError("incomplete_args")
		}
		terminal = true
		rem, err := decoder.Finish(int(t.cfg.MaxResponseBytes))
		if err != nil {
			return err
		}
		if err := emitContent(rem); err != nil {
			return err
		}
		return emitDelta(map[string]any{}, MapFinishReason(reason))
	}

	handleCall := func(fc *nativeFunctionCall) error {
		if terminal {
			return ProtocolError("data_after_finish")
		}
		if callClosed || (fc.Name != "" && fc.Name != t.toolName) {
			return ProtocolError("unexpected_call")
		}
		if !started {
			if fc.Name != t.toolName {
				return ProtocolError("missing_name")
			}
			started = true
			stats.SyntheticHit = true
			if err := emitDelta(map[string]any{"role": "assistant"}, nil); err != nil {
				return err
			}
		}
		// Complete (non-streaming) arguments arrive as a single object.
		if len(fc.Args) > 0 {
			if stringStarted || fullArgs {
				return ProtocolError("invalid_args")
			}
			fullArgs = true
			stringClosed = true
			if err := feed(string(fc.Args)); err != nil {
				return err
			}
		}
		for _, part := range fc.PartialArgs {
			if fullArgs || stringClosed || part.JSONPath != "$.content" {
				return ProtocolError("invalid_partial_args")
			}
			inner, ok := rawJSONStringInner(part.StringValue)
			if !ok {
				return ProtocolError("invalid_partial_args")
			}
			if !stringStarted {
				if err := feed(`{"content":"`); err != nil {
					return err
				}
				stringStarted = true
			}
			// The raw escaped bytes are fed verbatim so quotes, control
			// characters and split surrogate pairs survive unchanged.
			if inner != "" {
				if err := feed(inner); err != nil {
					return err
				}
			}
			if part.WillContinue == nil || !*part.WillContinue {
				if err := feed(`"`); err != nil {
					return err
				}
				stringClosed = true
			}
		}
		if fc.WillContinue == nil || !*fc.WillContinue {
			if !stringClosed {
				return ProtocolError("incomplete_args")
			}
			if !fullArgs {
				if err := feed(`}`); err != nil {
					return err
				}
			}
			callClosed = true
		}
		return nil
	}

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, ":") {
			continue
		}
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" {
			continue
		}
		if payload == "[DONE]" {
			break
		}
		var chunk nativeChunk
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			return stats, ProtocolError("invalid_sse")
		}
		if chunk.Error != nil {
			// Pass the upstream error object through, then stop without [DONE].
			_ = writeSSEData(map[string]any{"error": chunk.Error})
			return stats, fmt.Errorf("native upstream error: %s", chunk.Error.Message)
		}
		if chunk.UsageMetadata != nil {
			if translated := TranslateUsage(chunk.UsageMetadata); translated != nil {
				usage = translated
			}
		}
		if len(chunk.Candidates) > 1 {
			return stats, ProtocolError("multiple_candidates")
		}
		if len(chunk.Candidates) == 0 {
			if chunk.PromptFeedback.BlockReason != "" {
				// Any prompt block is a content filter, whatever its code.
				if err := finish("SAFETY"); err != nil {
					return stats, err
				}
			}
			continue
		}
		candidate := chunk.Candidates[0]
		for _, part := range candidate.Content.Parts {
			if terminal && (part.Text != nil || part.FunctionCall != nil) {
				return stats, ProtocolError("data_after_finish")
			}
			if part.FunctionCall != nil {
				if err := handleCall(part.FunctionCall); err != nil {
					return stats, err
				}
			}
			if part.Text != nil && *part.Text != "" {
				if part.Thought {
					if err := emitDelta(map[string]any{"reasoning_content": *part.Text}, nil); err != nil {
						return stats, err
					}
				} else {
					if err := emitContent(*part.Text); err != nil {
						return stats, err
					}
				}
			}
		}
		if candidate.FinishReason != "" {
			if err := finish(candidate.FinishReason); err != nil {
				return stats, err
			}
		}
	}
	if err := scanner.Err(); err != nil && err != io.EOF {
		return stats, fmt.Errorf("error reading native upstream stream: %w", err)
	}
	if !terminal {
		// The upstream ended without a terminal state; never present that as a
		// completed answer.
		return stats, ProtocolError("stream_interrupted")
	}
	if usage != nil {
		_ = writeSSEData(map[string]any{
			"id":      id,
			"object":  "chat.completion.chunk",
			"created": created,
			"model":   t.model,
			"choices": []any{},
			"usage":   usage,
		})
	}
	n, _ := io.WriteString(w, "data: [DONE]\n\n")
	stats.BytesWritten += int64(n)
	if flusher != nil {
		flusher.Flush()
	}
	return stats, nil
}
