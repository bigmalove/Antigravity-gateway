package recovery

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"antigravity-gateway/internal/config"
	"antigravity-gateway/internal/keymgmt"
	"antigravity-gateway/internal/proxy"
)

// nativeSSE renders one native SSE event.
func nativeSSE(v any) string {
	b, _ := json.Marshal(v)
	return "data: " + string(b) + "\n\n"
}

// TestNativeStreamEarlyDeliveryWhileUpstreamPaused is the canonical
// anti-truncation streaming test: the mock upstream stops after the first
// partialArgs segment and only finishes after the client has actually
// received that text through the gateway. It verifies early delivery (the
// client sees content while the upstream is still paused) and a complete,
// well-formed terminal state afterwards.
func TestNativeStreamEarlyDeliveryWhileUpstreamPaused(t *testing.T) {
	var (
		firstSegmentFlushed = make(chan struct{})
		release             = make(chan struct{})
		released            atomic.Bool
		nativeCalls         atomic.Int32
		compatCalls         atomic.Int32
		receivedPath        atomic.Value // string
		receivedBody        atomic.Value // map[string]any
	)

	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, ":streamGenerateContent") {
			nativeCalls.Add(1)
			receivedPath.Store(r.URL.Path)
			raw, _ := io.ReadAll(r.Body)
			var body map[string]any
			_ = json.Unmarshal(raw, &body)
			receivedBody.Store(body)

			// Find the synthetic declaration name.
			var toolName string
			if decls, ok := body["tools"].([]any); ok && len(decls) > 0 {
				if fd, ok := decls[0].(map[string]any)["functionDeclarations"].([]any); ok && len(fd) > 0 {
					toolName, _ = fd[0].(map[string]any)["name"].(string)
				}
			}

			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			flusher, _ := w.(http.Flusher)

			// 1. Open the transport call and deliver the first segment.
			_, _ = io.WriteString(w, nativeSSE(map[string]any{"candidates": []any{map[string]any{
				"content": map[string]any{"parts": []any{map[string]any{"functionCall": map[string]any{"name": toolName, "willContinue": true}}}},
			}}}))
			_, _ = io.WriteString(w, `data: {"candidates":[{"content":{"parts":[{"functionCall":{"partialArgs":[{"jsonPath":"$.content","stringValue":"Hello ","willContinue":true}],"willContinue":true}}]}}]}`+"\n\n")
			if flusher != nil {
				flusher.Flush()
			}
			close(firstSegmentFlushed)

			// 2. Stop here until the test confirms the client received the text.
			select {
			case <-release:
			case <-time.After(15 * time.Second):
				return
			}

			// 3. Finish the answer.
			_, _ = io.WriteString(w, `data: {"candidates":[{"content":{"parts":[{"functionCall":{"partialArgs":[{"jsonPath":"$.content","stringValue":"world!","willContinue":false}],"willContinue":false}}]}}]}`+"\n\n")
			_, _ = io.WriteString(w, nativeSSE(map[string]any{
				"candidates":    []any{map[string]any{"finishReason": "STOP"}},
				"usageMetadata": map[string]any{"promptTokenCount": 3, "candidatesTokenCount": 4, "totalTokenCount": 7},
			}))
			if flusher != nil {
				flusher.Flush()
			}
			return
		}
		// OpenAI-compatible fallback endpoint.
		compatCalls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, nativeSSE(map[string]any{"choices": []any{map[string]any{
			"index": 0, "delta": map[string]any{"role": "assistant", "content": "compat answer"}, "finish_reason": nil,
		}}}))
		_, _ = io.WriteString(w, nativeSSE(map[string]any{"choices": []any{map[string]any{
			"index": 0, "delta": map[string]any{}, "finish_reason": "stop",
		}}}))
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer mock.Close()

	cfg := &config.Config{
		UpstreamBaseURL:             mock.URL,
		UpstreamAPIKey:              "upstream-key",
		UpstreamAuthMode:            "bearer",
		UpstreamTimeout:             20 * time.Second,
		WrapperMode:                 "prefer",
		RecoveryPolicy:              "repair",
		SyntheticToolPrefix:         "agw_emit_",
		NativeStream:                true,
		NativeStreamURLTemplate:     mock.URL + "/v1beta/models/{model}:streamGenerateContent?alt=sse",
		NativeStreamAuthStyle:       "bearer",
		MaxRequestBytes:             1024 * 1024,
		MaxResponseBytes:            1024 * 1024,
		MaxConcurrentRequests:       10,
		MaxConcurrentRequestsPerKey: 10,
		RequestQueueTimeout:         time.Second,
	}

	keyMgr, err := keymgmt.NewManager(":memory:", "hmac-secret-at-least-16-bytes", nil)
	if err != nil {
		t.Fatalf("failed to create key manager: %v", err)
	}
	defer keyMgr.Close()

	client := proxy.NewUpstreamClient(cfg)
	orch := NewOrchestrator(cfg, client, keyMgr)

	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		ctx := context.WithValue(r.Context(), proxy.KeyInfoContextKey, &keymgmt.KeyInfo{ID: "k1", Status: "active"})
		ctx = context.WithValue(ctx, proxy.RequestIDContextKey, "req_native_early_delivery")
		orch.HandleChatCompletions(w, r.WithContext(ctx))
	})
	gw := httptest.NewServer(mux)
	defer gw.Close()

	reqBody := `{"model":"google/gemini-4-argon-thinking","stream":true,"max_tokens":512,"messages":[{"role":"user","content":"hi"}]}`
	resp, err := http.Post(gw.URL+"/v1/chat/completions", "application/json", strings.NewReader(reqBody))
	if err != nil {
		t.Fatalf("gateway request failed: %v", err)
	}
	defer resp.Body.Close()

	if got := resp.Header.Get("x-anti-truncation-transport"); got != "tool-transport-native-streaming" {
		t.Fatalf("x-anti-truncation-transport = %q", got)
	}

	type readResult struct {
		line string
		err  error
	}
	lines := make(chan readResult, 64)
	go func() {
		scanner := bufio.NewScanner(resp.Body)
		scanner.Buffer(make([]byte, 64*1024), 1024*1024)
		for scanner.Scan() {
			lines <- readResult{line: scanner.Text()}
		}
		lines <- readResult{err: scanner.Err()}
	}()

	contentOf := func(line string) string {
		if !strings.HasPrefix(strings.TrimSpace(line), "data:") {
			return ""
		}
		payload := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "data:"))
		if payload == "[DONE]" {
			return ""
		}
		var chunk map[string]any
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			return ""
		}
		choices, _ := chunk["choices"].([]any)
		if len(choices) == 0 {
			return ""
		}
		delta, _ := choices[0].(map[string]any)["delta"].(map[string]any)
		text, _ := delta["content"].(string)
		return text
	}

	// Phase 1: wait for the first segment while the upstream is paused. The
	// content can only arrive here if it was delivered early.
	var streamed strings.Builder
	firstSeen := make(chan struct{})
	deadline := time.After(10 * time.Second)
	waitFirst := true
	for waitFirst {
		select {
		case res := <-lines:
			if res.err != nil {
				t.Fatalf("stream ended before first content: %v", res.err)
			}
			if text := contentOf(res.line); text != "" {
				streamed.WriteString(text)
				waitFirst = false
			}
		case <-deadline:
			t.Fatalf("timed out waiting for the first content segment while the upstream was paused")
		}
	}
	close(firstSeen)
	if streamed.String() != "Hello " {
		t.Fatalf("first delivered content = %q, want %q", streamed.String(), "Hello ")
	}
	// The upstream must still be blocked at this point: the test has not
	// released it, so the content we just read was delivered early.
	select {
	case <-firstSegmentFlushed:
	default:
		t.Fatalf("mock upstream never flushed the first segment")
	}
	if released.Load() {
		t.Fatalf("upstream was already released before the client received content")
	}

	// Phase 2: let the upstream finish, then drain the stream.
	released.Store(true)
	close(release)

	finished := false
	done := false
	usageSeen := false
	timeout := time.After(10 * time.Second)
	for !done {
		select {
		case res := <-lines:
			if res.err != nil && res.err != io.EOF {
				t.Fatalf("stream error: %v", res.err)
			}
			if res.err != nil {
				done = true
				break
			}
			line := strings.TrimSpace(res.line)
			if line == "data: [DONE]" {
				done = true
				break
			}
			if text := contentOf(res.line); text != "" {
				streamed.WriteString(text)
			}
			if strings.HasPrefix(line, "data:") {
				payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
				var chunk map[string]any
				if err := json.Unmarshal([]byte(payload), &chunk); err == nil {
					if _, ok := chunk["usage"]; ok {
						usageSeen = true
					}
					if choices, ok := chunk["choices"].([]any); ok && len(choices) > 0 {
						if fr, ok := choices[0].(map[string]any)["finish_reason"].(string); ok && fr != "" {
							finished = true
							if fr != "stop" {
								t.Fatalf("finish_reason = %q, want stop", fr)
							}
						}
					}
				}
			}
		case <-timeout:
			t.Fatalf("timed out draining the rest of the stream")
		}
	}

	if got := streamed.String(); got != "Hello world!" {
		t.Fatalf("full content = %q, want %q", got, "Hello world!")
	}
	if !finished {
		t.Fatalf("no finish_reason stop observed")
	}
	if !usageSeen {
		t.Fatalf("no usage chunk observed")
	}
	if !done {
		t.Fatalf("stream did not terminate with [DONE]")
	}

	// The native call must carry the streaming function-argument switch and
	// the bare model name in the URL.
	if nativeCalls.Load() != 1 {
		t.Fatalf("native endpoint calls = %d, want 1", nativeCalls.Load())
	}
	if compatCalls.Load() != 0 {
		t.Fatalf("compat endpoint should not be called, got %d calls", compatCalls.Load())
	}
	path, _ := receivedPath.Load().(string)
	if !strings.Contains(path, "gemini-4-argon-thinking") || strings.Contains(path, "google/") {
		t.Fatalf("native URL path should use the bare model name: %s", path)
	}
	body, _ := receivedBody.Load().(map[string]any)
	if body == nil {
		t.Fatalf("no native request body captured")
	}
	toolConfig, _ := body["toolConfig"].(map[string]any)
	calling, _ := toolConfig["functionCallingConfig"].(map[string]any)
	if calling == nil || calling["streamFunctionCallArguments"] != true {
		t.Fatalf("streamFunctionCallArguments not enabled: %v", toolConfig)
	}
	if names, _ := calling["allowedFunctionNames"].([]any); len(names) != 1 || !strings.HasPrefix(names[0].(string), "agw_emit_") {
		t.Fatalf("allowedFunctionNames = %v", calling["allowedFunctionNames"])
	}
	if calling["mode"] != "AUTO" {
		t.Fatalf("mode = %v, want AUTO", calling["mode"])
	}
}

// TestNativeStreamMisconfiguredUpstreamGetsClearError: when NATIVE_STREAM is
// enabled against an upstream that only speaks the OpenAI-compatible API (it
// answers the native path with an HTML portal and HTTP 200), the client must
// receive an actionable 502 error instead of a silently empty stream.
func TestNativeStreamMisconfiguredUpstreamGetsClearError(t *testing.T) {
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Mimic an SPA/portal fallback for unknown native paths.
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "<!DOCTYPE html><html><head><title>AI 网关管理门户</title></head><body></body></html>")
	}))
	defer mock.Close()

	cfg := &config.Config{
		UpstreamBaseURL:             mock.URL,
		UpstreamTimeout:             10 * time.Second,
		WrapperMode:                 "prefer",
		RecoveryPolicy:              "repair",
		SyntheticToolPrefix:         "agw_emit_",
		NativeStream:                true,
		NativeStreamURLTemplate:     mock.URL + "/publishers/google/models/{model}:streamGenerateContent?alt=sse",
		NativeStreamAuthStyle:       "bearer",
		MaxRequestBytes:             1024 * 1024,
		MaxResponseBytes:            1024 * 1024,
		MaxConcurrentRequests:       10,
		MaxConcurrentRequestsPerKey: 10,
		RequestQueueTimeout:         time.Second,
	}
	keyMgr, err := keymgmt.NewManager(":memory:", "hmac-secret-at-least-16-bytes", nil)
	if err != nil {
		t.Fatalf("failed to create key manager: %v", err)
	}
	defer keyMgr.Close()
	client := proxy.NewUpstreamClient(cfg)
	orch := NewOrchestrator(cfg, client, keyMgr)

	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		ctx := context.WithValue(r.Context(), proxy.KeyInfoContextKey, &keymgmt.KeyInfo{ID: "k1", Status: "active"})
		ctx = context.WithValue(ctx, proxy.RequestIDContextKey, "req_native_misconfig")
		orch.HandleChatCompletions(w, r.WithContext(ctx))
	})
	gw := httptest.NewServer(mux)
	defer gw.Close()

	reqBody := `{"model":"google/gemini-4-argon-thinking","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	resp, err := http.Post(gw.URL+"/v1/chat/completions", "application/json", strings.NewReader(reqBody))
	if err != nil {
		t.Fatalf("gateway request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", resp.StatusCode)
	}
	raw, _ := io.ReadAll(resp.Body)
	body := string(raw)
	if !strings.Contains(body, "native_upstream_not_sse") {
		t.Fatalf("error code missing, body: %s", body)
	}
	if !strings.Contains(body, "NATIVE_STREAM") {
		t.Fatalf("actionable hint missing, body: %s", body)
	}
	if strings.Contains(body, "[DONE]") {
		t.Fatalf("misconfig must not produce a stream, body: %s", body)
	}
}

// TestNativeStreamDisabledKeepsCompatibleTransport: with NATIVE_STREAM off the
// gateway must keep using the OpenAI-compatible synthetic transport.
func TestNativeStreamDisabledKeepsCompatibleTransport(t *testing.T) {
	var nativeCalls, compatCalls atomic.Int32
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, ":streamGenerateContent") {
			nativeCalls.Add(1)
			http.NotFound(w, r)
			return
		}
		compatCalls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, nativeSSE(map[string]any{"choices": []any{map[string]any{
			"index": 0, "delta": map[string]any{"role": "assistant", "content": "compat answer"}, "finish_reason": nil,
		}}}))
		_, _ = io.WriteString(w, nativeSSE(map[string]any{"choices": []any{map[string]any{
			"index": 0, "delta": map[string]any{}, "finish_reason": "stop",
		}}}))
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer mock.Close()

	cfg := &config.Config{
		UpstreamBaseURL:             mock.URL,
		UpstreamTimeout:             10 * time.Second,
		WrapperMode:                 "prefer",
		RecoveryPolicy:              "repair",
		SyntheticToolPrefix:         "agw_emit_",
		NativeStream:                false, // disabled on purpose
		MaxRequestBytes:             1024 * 1024,
		MaxResponseBytes:            1024 * 1024,
		MaxConcurrentRequests:       10,
		MaxConcurrentRequestsPerKey: 10,
		RequestQueueTimeout:         time.Second,
	}
	keyMgr, err := keymgmt.NewManager(":memory:", "hmac-secret-at-least-16-bytes", nil)
	if err != nil {
		t.Fatalf("failed to create key manager: %v", err)
	}
	defer keyMgr.Close()
	client := proxy.NewUpstreamClient(cfg)
	orch := NewOrchestrator(cfg, client, keyMgr)

	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		ctx := context.WithValue(r.Context(), proxy.KeyInfoContextKey, &keymgmt.KeyInfo{ID: "k1", Status: "active"})
		ctx = context.WithValue(ctx, proxy.RequestIDContextKey, "req_native_disabled")
		orch.HandleChatCompletions(w, r.WithContext(ctx))
	})
	gw := httptest.NewServer(mux)
	defer gw.Close()

	reqBody := `{"model":"google/gemini-4-argon-thinking","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	resp, err := http.Post(gw.URL+"/v1/chat/completions", "application/json", strings.NewReader(reqBody))
	if err != nil {
		t.Fatalf("gateway request failed: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(raw), "compat answer") {
		t.Fatalf("expected compatible transport answer, got: %s", string(raw))
	}
	if nativeCalls.Load() != 0 {
		t.Fatalf("native endpoint must not be called when disabled, got %d", nativeCalls.Load())
	}
	if compatCalls.Load() != 1 {
		t.Fatalf("compat endpoint calls = %d, want 1", compatCalls.Load())
	}
	if got := resp.Header.Get("x-anti-truncation-transport"); got != "" {
		t.Fatalf("no transport header expected on the compatible path, got %q", got)
	}
}
