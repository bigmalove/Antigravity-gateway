package native

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"antigravity-gateway/internal/config"
)

const testToolName = "agw_emit_native_test"

func testConfig() *config.Config {
	return &config.Config{MaxResponseBytes: 16 * 1024 * 1024}
}

func sseEvent(v any) string {
	b, _ := json.Marshal(v)
	return "data: " + string(b) + "\n\n"
}

func callEvent(fc map[string]any) string {
	return sseEvent(map[string]any{
		"candidates": []any{
			map[string]any{"content": map[string]any{"parts": []any{map[string]any{"functionCall": fc}}}},
		},
	})
}

// partEvent builds a partialArgs event with pre-escaped fragment text (the
// escaped form is what the upstream wire carries, like JS
// JSON.stringify(v).slice(1, -1)).
func partEvent(escaped string, willContinue bool) string {
	return fmt.Sprintf(
		`data: {"candidates":[{"content":{"parts":[{"functionCall":{"partialArgs":[{"jsonPath":"$.content","stringValue":"%s","willContinue":%t}],"willContinue":true}}]}}]}`+"\n\n",
		escaped, willContinue)
}

func startEvent() string {
	return callEvent(map[string]any{"name": testToolName, "willContinue": true})
}

func closeCallEvent() string {
	return callEvent(map[string]any{})
}

func finishEvent(reason string) string {
	return sseEvent(map[string]any{
		"candidates": []any{map[string]any{"finishReason": reason}},
		"usageMetadata": map[string]any{
			"promptTokenCount": 12, "candidatesTokenCount": 8, "thoughtsTokenCount": 3,
			"totalTokenCount": 23, "trafficType": "ON_DEMAND",
		},
	})
}

func escapeFragment(s string) string {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(s); err != nil {
		panic(err)
	}
	out := buf.String()
	// Strip the leading quote, trailing quote and encoder newline.
	return out[1 : len(out)-2]
}

func run(t *testing.T, wire string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	tr := NewStreamTransformer(testConfig(), testToolName, "gemini-test-model")
	_, err := tr.Transform(strings.NewReader(wire), &out, nil)
	return out.String(), err
}

func parseEvents(t *testing.T, out string) []map[string]any {
	t.Helper()
	var events []map[string]any
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "[DONE]" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(payload), &m); err != nil {
			t.Fatalf("invalid SSE json %q: %v", payload, err)
		}
		events = append(events, m)
	}
	return events
}

func collectContent(events []map[string]any) string {
	var b strings.Builder
	for _, e := range events {
		choices, _ := e["choices"].([]any)
		if len(choices) == 0 {
			continue
		}
		choice, _ := choices[0].(map[string]any)
		delta, _ := choice["delta"].(map[string]any)
		if c, ok := delta["content"].(string); ok {
			b.WriteString(c)
		}
	}
	return b.String()
}

// TestPartialArgsRestore mirrors the reference test: quotes, control
// characters and split surrogate pairs must round-trip, the wrapper must never
// surface tool_calls, usage is translated and a genuine stop stays a stop.
func TestPartialArgsRestore(t *testing.T) {
	fragments := []string{"中文 \"quoted\" \\ path\n", "\t<gametxt>first"}
	wire := startEvent() +
		partEvent(escapeFragment(fragments[0]), true) +
		partEvent(escapeFragment(fragments[1]), true) +
		// Split surrogate pair: high and low halves arrive separately.
		partEvent(`\ud83d`, true) +
		partEvent(`\ude00</gametxt>`, true) +
		partEvent("", false) + // closes the string
		closeCallEvent() + // closes the call
		finishEvent("STOP")

	out, err := run(t, wire)
	if err != nil {
		t.Fatalf("Transform error: %v", err)
	}
	events := parseEvents(t, out)
	expected := "中文 \"quoted\" \\ path\n" + "\t<gametxt>first" + "\U0001F600" + "</gametxt>"
	if got := collectContent(events); got != expected {
		t.Fatalf("content mismatch:\n got %q\nwant %q", got, expected)
	}
	if strings.Contains(out, "tool_calls") {
		t.Fatalf("tool_calls leaked into client stream: %s", out)
	}
	// Exactly one role announcement.
	roles := 0
	for _, e := range events {
		choices, _ := e["choices"].([]any)
		if len(choices) == 0 {
			continue
		}
		choice, _ := choices[0].(map[string]any)
		delta, _ := choice["delta"].(map[string]any)
		if r, ok := delta["role"].(string); ok && r != "" {
			roles++
		}
	}
	if roles != 1 {
		t.Fatalf("expected exactly 1 role chunk, got %d", roles)
	}
	// finish_reason stop.
	finished := false
	for _, e := range events {
		choices, _ := e["choices"].([]any)
		if len(choices) == 0 {
			continue
		}
		choice, _ := choices[0].(map[string]any)
		if fr, ok := choice["finish_reason"].(string); ok && fr != "" {
			finished = true
			if fr != "stop" {
				t.Fatalf("finish_reason = %q, want stop", fr)
			}
		}
	}
	if !finished {
		t.Fatalf("no finish_reason chunk emitted")
	}
	// Usage chunk translated.
	foundUsage := false
	for _, e := range events {
		if u, ok := e["usage"].(map[string]any); ok {
			foundUsage = true
			if u["prompt_tokens"].(float64) != 12 || u["completion_tokens"].(float64) != 11 || u["total_tokens"].(float64) != 23 {
				t.Fatalf("unexpected usage: %v", u)
			}
		}
	}
	if !foundUsage {
		t.Fatalf("no usage chunk emitted")
	}
	if !strings.HasSuffix(out, "data: [DONE]\n\n") {
		t.Fatalf("stream does not end with [DONE]")
	}
}

// TestFullArgsFallback: a complete (non-streaming) args object still restores.
func TestFullArgsFallback(t *testing.T) {
	wire := callEvent(map[string]any{"name": testToolName, "args": map[string]any{"content": "complete"}}) +
		finishEvent("STOP")
	out, err := run(t, wire)
	if err != nil {
		t.Fatalf("Transform error: %v", err)
	}
	if got := collectContent(parseEvents(t, out)); got != "complete" {
		t.Fatalf("content = %q, want complete", got)
	}
}

// TestTruncationCannotBecomeSuccess: MAX_TOKENS/SAFETY keep their mapped
// finish_reason and partial content, while every interrupted or malformed
// variant fails without [DONE].
func TestTruncationCannotBecomeSuccess(t *testing.T) {
	for _, tc := range []struct{ reason, want string }{{"MAX_TOKENS", "length"}, {"SAFETY", "content_filter"}} {
		wire := startEvent() + partEvent("partial", true) + finishEvent(tc.reason)
		out, err := run(t, wire)
		if err != nil {
			t.Fatalf("%s: unexpected error: %v", tc.reason, err)
		}
		if got := collectContent(parseEvents(t, out)); got != "partial" {
			t.Fatalf("%s: content = %q, want partial", tc.reason, got)
		}
		wantFound := false
		for _, e := range parseEvents(t, out) {
			choices, _ := e["choices"].([]any)
			if len(choices) == 0 {
				continue
			}
			choice, _ := choices[0].(map[string]any)
			if fr, ok := choice["finish_reason"].(string); ok && fr != "" {
				wantFound = true
				if fr != tc.want {
					t.Fatalf("%s: finish_reason = %q, want %q", tc.reason, fr, tc.want)
				}
			}
		}
		if !wantFound {
			t.Fatalf("%s: no finish chunk", tc.reason)
		}
	}

	bad := []struct {
		name string
		wire string
	}{
		{"stop_with_open_args", startEvent() + partEvent("x", true) + finishEvent("STOP")},
		{"stop_with_open_string", startEvent() + partEvent("x", false) + finishEvent("STOP")},
		{"interrupted_without_finish", startEvent() + partEvent("x", false) + closeCallEvent()},
		{"duplicate_call_after_close", startEvent() + partEvent("x", false) + closeCallEvent() +
			callEvent(map[string]any{"name": testToolName, "args": map[string]any{"content": "dup"}}) + finishEvent("STOP")},
		{"wrong_json_path", startEvent() + partEvent(`bad`, true) + finishEvent("STOP")},
		{"unexpected_tool_name", callEvent(map[string]any{"name": "other_tool", "willContinue": true}) + finishEvent("STOP")},
		{"missing_name", callEvent(map[string]any{"partialArgs": []any{map[string]any{"jsonPath": "$.content", "stringValue": "x", "willContinue": false}}, "willContinue": false}) + finishEvent("STOP")},
		{"data_after_finish", startEvent() + partEvent("z", false) + closeCallEvent() + finishEvent("STOP") + partEvent("late", true)},
		{"duplicate_finish", startEvent() + partEvent("z", false) + closeCallEvent() + finishEvent("STOP") + finishEvent("STOP")},
		{"invalid_sse", "data: not-json\n\n"},
	}
	for _, tc := range bad {
		out, err := run(t, tc.wire)
		if err == nil {
			t.Fatalf("%s: expected protocol error, got success (out=%q)", tc.name, out)
		}
		if !strings.Contains(err.Error(), "anti_truncation_native_") {
			t.Fatalf("%s: error %v does not carry the native protocol family", tc.name, err)
		}
		if strings.Contains(out, "[DONE]") {
			t.Fatalf("%s: emitted [DONE] despite failure", tc.name)
		}
	}

	// A wrong jsonPath (not "$.content") must also fail when the string already started.
	out, err := run(t, fmt.Sprintf(
		`data: {"candidates":[{"content":{"parts":[{"functionCall":{"partialArgs":[{"jsonPath":"$.other","stringValue":"bad","willContinue":true}],"willContinue":true}}]}}]}`+"\n\n")+
		startEvent()+finishEvent("STOP"))
	if err == nil {
		t.Fatalf("wrong jsonPath: expected error, got %q", out)
	}
}

// TestPromptBlockBecomesContentFilter: a candidate-less prompt block maps to
// finish_reason content_filter.
func TestPromptBlockBecomesContentFilter(t *testing.T) {
	wire := sseEvent(map[string]any{"promptFeedback": map[string]any{"blockReason": "OTHER"}})
	out, err := run(t, wire)
	if err != nil {
		t.Fatalf("Transform error: %v", err)
	}
	found := false
	for _, e := range parseEvents(t, out) {
		choices, _ := e["choices"].([]any)
		if len(choices) == 0 {
			continue
		}
		choice, _ := choices[0].(map[string]any)
		if fr, ok := choice["finish_reason"].(string); ok && fr == "content_filter" {
			found = true
		}
	}
	if !found {
		t.Fatalf("prompt block did not map to content_filter: %s", out)
	}
}

func injectedTextBody() map[string]any {
	return map[string]any{
		"model": "google/gemini-4-argon-thinking",
		"messages": []any{
			map[string]any{"role": "system", "content": "sys prompt"},
			map[string]any{"role": "user", "content": "hi"},
			map[string]any{"role": "user", "content": "more"},
			map[string]any{"role": "assistant", "content": "prev"},
			map[string]any{"role": "user", "content": "now"},
		},
		"stream":            true,
		"max_tokens":        float64(512),
		"temperature":       0.6,
		"top_p":             0.9,
		"stop":              []any{"END"},
		"frequency_penalty": 0.1,
		"tools": []any{
			map[string]any{"type": "function", "function": map[string]any{
				"name": testToolName, "description": "transport", "parameters": map[string]any{"type": "object"},
			}},
		},
		"tool_choice": "auto",
	}
}

func TestSupportsNativeTextStream(t *testing.T) {
	if !SupportsNativeTextStream(injectedTextBody(), testToolName) {
		t.Fatalf("expected translatable body to be supported")
	}
	negatives := []struct {
		name   string
		mutate func(map[string]any)
	}{
		{"extra_field", func(b map[string]any) { b["logprobs"] = true }},
		{"unknown_extra", func(b map[string]any) { b["custom_thing"] = 1 }},
		{"unknown_stream_options", func(b map[string]any) { b["stream_options"] = map[string]any{"weird": true} }},
		{"message_name_metadata", func(b map[string]any) {
			b["messages"] = []any{map[string]any{"role": "user", "content": "x", "name": "meta"}}
		}},
		{"assistant_tool_calls", func(b map[string]any) {
			b["messages"] = []any{map[string]any{"role": "assistant", "content": "", "tool_calls": []any{}}}
		}},
		{"image_part", func(b map[string]any) {
			b["messages"] = []any{map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "image_url", "image_url": map[string]any{"url": "https://x.invalid/i.png"}}}}}
		}},
		{"two_tools", func(b map[string]any) {
			b["tools"] = []any{b["tools"].([]any)[0], map[string]any{"type": "function", "function": map[string]any{"name": "real"}}}
		}},
		{"wrong_tool_name", func(b map[string]any) {
			b["tools"] = []any{map[string]any{"type": "function", "function": map[string]any{"name": "nope"}}}
		}},
		{"forced_tool_choice_object", func(b map[string]any) {
			b["tool_choice"] = map[string]any{"type": "function", "function": map[string]any{"name": testToolName}}
		}},
		{"tool_role_message", func(b map[string]any) {
			b["messages"] = []any{map[string]any{"role": "tool", "content": "result", "tool_call_id": "c1"}}
		}},
	}
	for _, tc := range negatives {
		body := injectedTextBody()
		tc.mutate(body)
		if SupportsNativeTextStream(body, testToolName) {
			t.Fatalf("%s: expected unsupported", tc.name)
		}
	}
}

func TestBuildNativeTextBody(t *testing.T) {
	body := injectedTextBody()
	nativeBody, err := BuildNativeTextBody(body, testToolName)
	if err != nil {
		t.Fatalf("BuildNativeTextBody error: %v", err)
	}
	// The wire that enables incremental function arguments.
	toolConfig := nativeBody["toolConfig"].(map[string]any)
	calling := toolConfig["functionCallingConfig"].(map[string]any)
	if calling["streamFunctionCallArguments"] != true {
		t.Fatalf("streamFunctionCallArguments not enabled: %v", calling)
	}
	if names := calling["allowedFunctionNames"].([]any); len(names) != 1 || names[0] != testToolName {
		t.Fatalf("allowedFunctionNames = %v", names)
	}
	if calling["mode"] != "AUTO" {
		t.Fatalf("mode = %v, want AUTO for tool_choice auto", calling["mode"])
	}
	// Declaration carries the synthetic schema.
	decls := nativeBody["tools"].([]any)[0].(map[string]any)["functionDeclarations"].([]any)
	if decls[0].(map[string]any)["name"] != testToolName {
		t.Fatalf("declaration name mismatch: %v", decls)
	}
	// generationConfig translation.
	gc := nativeBody["generationConfig"].(map[string]any)
	if gc["maxOutputTokens"] != int64(512) || gc["temperature"] != 0.6 || gc["topP"] != 0.9 {
		t.Fatalf("generationConfig mismatch: %v", gc)
	}
	if stops, ok := gc["stopSequences"].([]any); !ok || len(stops) != 1 || stops[0] != "END" {
		t.Fatalf("stopSequences mismatch: %v", gc["stopSequences"])
	}
	if gc["frequencyPenalty"] != 0.1 {
		t.Fatalf("frequencyPenalty mismatch: %v", gc)
	}
	// systemInstruction only holds the leading system message.
	sys := nativeBody["systemInstruction"].(map[string]any)["parts"].([]map[string]any)
	if len(sys) != 1 || sys[0]["text"] != "sys prompt" {
		t.Fatalf("systemInstruction mismatch: %v", sys)
	}
	// Contents merge consecutive same-role turns.
	contents := nativeBody["contents"].([]map[string]any)
	if len(contents) != 3 {
		t.Fatalf("contents length = %d, want 3: %v", len(contents), contents)
	}
	first := contents[0]
	if first["role"] != "user" || len(first["parts"].([]map[string]any)) != 2 {
		t.Fatalf("first content should merge two user turns: %v", first)
	}
	if contents[1]["role"] != "model" {
		t.Fatalf("second content should be model: %v", contents[1])
	}
	// tool_choice required -> ANY
	required := injectedTextBody()
	required["tool_choice"] = "required"
	requiredBody, _ := BuildNativeTextBody(required, testToolName)
	if requiredBody["toolConfig"].(map[string]any)["functionCallingConfig"].(map[string]any)["mode"] != "ANY" {
		t.Fatalf("tool_choice required should map to ANY")
	}
}

func TestNativeStreamURL(t *testing.T) {
	cfg := &config.Config{UpstreamBaseURL: "https://us-central1-aiplatform.googleapis.com/v1/projects/p/locations/us-central1/endpoints/openapi"}
	got := cfg.NativeStreamURL("google/gemini-4-argon-thinking")
	want := "https://us-central1-aiplatform.googleapis.com/v1/projects/p/locations/us-central1/publishers/google/models/gemini-4-argon-thinking:streamGenerateContent?alt=sse"
	if got != want {
		t.Fatalf("default URL:\n got %s\nwant %s", got, want)
	}
	cfg2 := &config.Config{
		UpstreamBaseURL:         "https://example.com",
		NativeStreamURLTemplate: "https://gw.example.com/v1beta/models/{model}:streamGenerateContent?alt=sse",
	}
	if got := cfg2.NativeStreamURL("google/gemini-4-argon-thinking"); got != "https://gw.example.com/v1beta/models/gemini-4-argon-thinking:streamGenerateContent?alt=sse" {
		t.Fatalf("template URL mismatch: %s", got)
	}
}

var _ = http.StatusOK
