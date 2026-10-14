// Package native implements the Vertex/Gemini native streaming transport for
// anti-truncation. Reference: https://github.com/ken050210/vertex-streaming-anti-truncation
//
// The OpenAI-compatible endpoint of some upstreams buffers a tool call until
// its arguments are complete, so clients receive the whole answer at once even
// though the gateway parses SSE incrementally. For translatable text requests
// this package translates the (already synthetic-tool-injected) OpenAI request
// into the native streamGenerateContent format with
// streamFunctionCallArguments enabled, and restores the incremental
// partialArgs back into ordinary OpenAI delta.content chunks, so a segment is
// delivered as soon as it is produced.
package native

import (
	"encoding/json"
	"fmt"
	"strings"
)

// allowedRequestFields lists the OpenAI request fields that translate to the
// native format without loss. Anything else keeps the request on the
// OpenAI-compatible transport (buffered-fields fallback).
var allowedRequestFields = map[string]bool{
	"model":                 true,
	"messages":              true,
	"stream":                true,
	"max_tokens":            true,
	"max_completion_tokens": true,
	"temperature":           true,
	"top_p":                 true,
	"top_k":                 true,
	"presence_penalty":      true,
	"frequency_penalty":     true,
	"seed":                  true,
	"stop":                  true,
	"tools":                 true,
	"tool_choice":           true,
	"stream_options":        true,
}

var allowedStreamOptions = map[string]bool{"include_usage": true}

// SupportsNativeTextStream reports whether an injected OpenAI request body can
// be translated to the native text-stream format without discarding fields or
// message metadata. The body must already carry exactly one tool (the
// synthetic transport tool) and no downstream tools.
func SupportsNativeTextStream(body map[string]any, syntheticToolName string) bool {
	if syntheticToolName == "" {
		return false
	}
	for key, value := range body {
		if value == nil {
			continue
		}
		if !allowedRequestFields[key] {
			return false
		}
	}
	if opts, ok := body["stream_options"]; ok && opts != nil {
		m, ok := opts.(map[string]any)
		if !ok {
			return false
		}
		for k := range m {
			if !allowedStreamOptions[k] {
				return false
			}
		}
	}
	// tools must be exactly the injected synthetic tool.
	tools, _ := body["tools"].([]any)
	if len(tools) != 1 {
		return false
	}
	tool, _ := tools[0].(map[string]any)
	fn, _ := tool["function"].(map[string]any)
	if name, _ := fn["name"].(string); name != syntheticToolName {
		return false
	}
	// tool_choice: only the string forms auto/required/none translate; a forced
	// function object would fight the synthetic ANY/auto mapping.
	switch choice := body["tool_choice"].(type) {
	case nil:
	case string:
		switch strings.ToLower(choice) {
		case "auto", "required", "none":
		default:
			return false
		}
	default:
		return false
	}
	// messages must be plain text turns (role + string/plain-text content).
	messages, ok := body["messages"].([]any)
	if !ok || len(messages) == 0 {
		return false
	}
	for _, raw := range messages {
		msg, ok := raw.(map[string]any)
		if !ok {
			return false
		}
		switch role, _ := msg["role"].(string); role {
		case "system", "developer", "user", "assistant":
		default:
			return false
		}
		for key, value := range msg {
			if value == nil || key == "role" || key == "content" {
				continue
			}
			// name, tool_calls, tool_call_id, ... would be discarded by the
			// native translation; keep the compatible transport instead.
			return false
		}
		if !isTranslatableContent(msg["content"]) {
			return false
		}
	}
	return true
}

func isTranslatableContent(content any) bool {
	if content == nil {
		return true
	}
	switch v := content.(type) {
	case string:
		return true
	case []any:
		for _, raw := range v {
			part, ok := raw.(map[string]any)
			if !ok {
				return false
			}
			for key, value := range part {
				if value == nil || key == "type" || key == "text" {
					continue
				}
				return false
			}
			if t, _ := part["type"].(string); t != "text" {
				return false
			}
			if _, ok := part["text"].(string); !ok {
				return false
			}
		}
		return true
	default:
		return false
	}
}

func contentParts(content any) []map[string]any {
	switch v := content.(type) {
	case string:
		if v == "" {
			return nil
		}
		return []map[string]any{{"text": v}}
	case []any:
		parts := make([]map[string]any, 0, len(v))
		for _, raw := range v {
			part, _ := raw.(map[string]any)
			text, _ := part["text"].(string)
			if text != "" {
				parts = append(parts, map[string]any{"text": text})
			}
		}
		return parts
	}
	return nil
}

// BuildNativeTextBody translates an injected, translatable OpenAI request into
// the native streamGenerateContent body with streamFunctionCallArguments
// enabled. Only call it after SupportsNativeTextStream returned true.
func BuildNativeTextBody(body map[string]any, syntheticToolName string) (map[string]any, error) {
	messages, _ := body["messages"].([]any)

	// Only the leading instructions become systemInstruction; later system
	// messages stay in place as user text (SillyTavern-style behaviour).
	var systemParts []map[string]any
	contents := make([]map[string]any, 0, len(messages))
	leading := true
	for _, raw := range messages {
		msg, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		role, _ := msg["role"].(string)
		if leading && (role == "system" || role == "developer") {
			systemParts = append(systemParts, contentParts(msg["content"])...)
			continue
		}
		leading = false
		parts := contentParts(msg["content"])
		if len(parts) == 0 {
			continue
		}
		nativeRole := "user"
		if role == "assistant" {
			nativeRole = "model"
		}
		if n := len(contents); n > 0 && contents[n-1]["role"] == nativeRole {
			merged := contents[n-1]["parts"].([]map[string]any)
			contents[n-1]["parts"] = append(merged, parts...)
		} else {
			contents = append(contents, map[string]any{"role": nativeRole, "parts": parts})
		}
	}
	if len(contents) == 0 {
		contents = append(contents, map[string]any{"role": "user", "parts": []map[string]any{{"text": " "}}})
	}

	generationConfig := map[string]any{}
	if v, ok := intField(body, "max_tokens", "max_completion_tokens"); ok {
		generationConfig["maxOutputTokens"] = v
	}
	if v, ok := floatField(body, "temperature"); ok {
		generationConfig["temperature"] = v
	}
	if v, ok := floatField(body, "top_p"); ok {
		generationConfig["topP"] = v
	}
	if v, ok := floatField(body, "top_k"); ok {
		generationConfig["topK"] = int64(v)
	}
	if v, ok := floatField(body, "presence_penalty"); ok {
		generationConfig["presencePenalty"] = v
	}
	if v, ok := floatField(body, "frequency_penalty"); ok {
		generationConfig["frequencyPenalty"] = v
	}
	if v, ok := floatField(body, "seed"); ok {
		generationConfig["seed"] = int64(v)
	}
	if stop, ok := body["stop"]; ok && stop != nil {
		switch s := stop.(type) {
		case string:
			if s != "" {
				generationConfig["stopSequences"] = []any{s}
			}
		case []any:
			if len(s) > 0 {
				generationConfig["stopSequences"] = s
			}
		}
	}

	// Synthetic tool declaration (guaranteed present by the eligibility check).
	tools, _ := body["tools"].([]any)
	tool, _ := tools[0].(map[string]any)
	fn, _ := tool["function"].(map[string]any)
	parameters := fn["parameters"]
	if parameters == nil {
		parameters = map[string]any{"type": "object", "properties": map[string]any{}}
	}
	declaration := map[string]any{
		"name":                 syntheticToolName,
		"description":          stringOr(fn["description"], ""),
		"parametersJsonSchema": parameters,
	}

	mode := "AUTO"
	if choice, _ := body["tool_choice"].(string); strings.EqualFold(choice, "required") {
		mode = "ANY"
	}

	bodyOut := map[string]any{
		"contents":         contents,
		"generationConfig": generationConfig,
		"tools": []any{
			map[string]any{"functionDeclarations": []any{declaration}},
		},
		"toolConfig": map[string]any{
			"functionCallingConfig": map[string]any{
				"mode":                        mode,
				"allowedFunctionNames":        []any{syntheticToolName},
				"streamFunctionCallArguments": true,
			},
		},
	}
	if len(systemParts) > 0 {
		bodyOut["systemInstruction"] = map[string]any{"parts": systemParts}
	}
	return bodyOut, nil
}

func intField(body map[string]any, keys ...string) (int64, bool) {
	for _, key := range keys {
		if v, ok := floatField(body, key); ok {
			return int64(v), true
		}
	}
	return 0, false
}

func floatField(body map[string]any, key string) (float64, bool) {
	v, ok := body[key]
	if !ok || v == nil {
		return 0, false
	}
	switch n := v.(type) {
	case float64:
		return n, true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	default:
		return 0, false
	}
}

func stringOr(v any, fallback string) string {
	if s, ok := v.(string); ok {
		return s
	}
	return fallback
}

// MapFinishReason maps a native finishReason (or SAFETY for a prompt block)
// onto an OpenAI-compatible finish_reason.
func MapFinishReason(reason string) string {
	switch reason {
	case "STOP":
		return "stop"
	case "MAX_TOKENS":
		return "length"
	case "SAFETY", "PROHIBITED_CONTENT", "SPII", "BLOCKLIST", "RECITATION", "IMAGE_SAFETY":
		return "content_filter"
	case "":
		return "stop"
	default:
		return strings.ToLower(reason)
	}
}

// TranslateUsage converts a native usageMetadata object into an
// OpenAI-compatible usage object. Reported completion tokens include thoughts,
// mirroring the reference implementation.
func TranslateUsage(raw json.RawMessage) map[string]any {
	var meta struct {
		PromptTokenCount        float64 `json:"promptTokenCount"`
		CandidatesTokenCount    float64 `json:"candidatesTokenCount"`
		ThoughtsTokenCount      float64 `json:"thoughtsTokenCount"`
		TotalTokenCount         float64 `json:"totalTokenCount"`
		CachedContentTokenCount float64 `json:"cachedContentTokenCount"`
		TrafficType             string  `json:"trafficType"`
	}
	if err := json.Unmarshal(raw, &meta); err != nil {
		return nil
	}
	prompt := int64(meta.PromptTokenCount)
	completion := int64(meta.CandidatesTokenCount + meta.ThoughtsTokenCount)
	total := int64(meta.TotalTokenCount)
	if total == 0 {
		total = prompt + completion
	}
	usage := map[string]any{
		"prompt_tokens":     prompt,
		"completion_tokens": completion,
		"total_tokens":      total,
		"prompt_tokens_details": map[string]any{
			"cached_tokens": int64(meta.CachedContentTokenCount),
		},
	}
	if meta.TrafficType != "" {
		usage["traffic_type"] = meta.TrafficType
	}
	return usage
}

// ProtocolError formats a native-transport protocol violation, mirroring the
// anti_truncation_native_* error family of the reference implementation.
func ProtocolError(suffix string) error {
	return fmt.Errorf("anti_truncation_native_%s", suffix)
}
