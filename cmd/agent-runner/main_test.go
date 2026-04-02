package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"
)

// sseWrite writes a single SSE data line and flushes.
func sseWrite(w http.ResponseWriter, data string) {
	fmt.Fprintf(w, "data: %s\n\n", data)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

// sseWriteEvent writes an SSE event with type and data.
func sseWriteEvent(w http.ResponseWriter, eventType, data string) {
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", eventType, data)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

// writeOpenAISSE writes an OpenAI-compatible streaming response for a simple
// text completion.
func writeOpenAISSE(w http.ResponseWriter, id, model, content, finishReason string, promptTok, completionTok int) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)

	chunk1, _ := json.Marshal(map[string]any{
		"id": id, "object": "chat.completion.chunk", "model": model,
		"choices": []map[string]any{{
			"index": 0, "delta": map[string]string{"role": "assistant"}, "finish_reason": nil,
		}},
	})
	sseWrite(w, string(chunk1))

	chunk2, _ := json.Marshal(map[string]any{
		"id": id, "object": "chat.completion.chunk", "model": model,
		"choices": []map[string]any{{
			"index": 0, "delta": map[string]string{"content": content}, "finish_reason": nil,
		}},
	})
	sseWrite(w, string(chunk2))

	chunk3, _ := json.Marshal(map[string]any{
		"id": id, "object": "chat.completion.chunk", "model": model,
		"choices": []map[string]any{{
			"index": 0, "delta": map[string]any{}, "finish_reason": finishReason,
		}},
		"usage": map[string]int{
			"prompt_tokens": promptTok, "completion_tokens": completionTok,
			"total_tokens": promptTok + completionTok,
		},
	})
	sseWrite(w, string(chunk3))
	sseWrite(w, "[DONE]")
}

// writeAnthropicSSEText writes an Anthropic-compatible streaming response for a
// simple text message.
func writeAnthropicSSEText(w http.ResponseWriter, id, model, text, stopReason string, inputTok, outputTok int) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)

	msgStart, _ := json.Marshal(map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id": id, "type": "message", "role": "assistant", "model": model,
			"content": []any{}, "stop_reason": nil,
			"usage": map[string]int{"input_tokens": inputTok, "output_tokens": 0},
		},
	})
	sseWriteEvent(w, "message_start", string(msgStart))

	cbStart, _ := json.Marshal(map[string]any{
		"type": "content_block_start", "index": 0,
		"content_block": map[string]string{"type": "text", "text": ""},
	})
	sseWriteEvent(w, "content_block_start", string(cbStart))

	cbDelta, _ := json.Marshal(map[string]any{
		"type": "content_block_delta", "index": 0,
		"delta": map[string]string{"type": "text_delta", "text": text},
	})
	sseWriteEvent(w, "content_block_delta", string(cbDelta))

	cbStop, _ := json.Marshal(map[string]any{"type": "content_block_stop", "index": 0})
	sseWriteEvent(w, "content_block_stop", string(cbStop))

	msgDelta, _ := json.Marshal(map[string]any{
		"type":  "message_delta",
		"delta": map[string]string{"stop_reason": stopReason},
		"usage": map[string]int{"output_tokens": outputTok},
	})
	sseWriteEvent(w, "message_delta", string(msgDelta))

	msgStop, _ := json.Marshal(map[string]any{"type": "message_stop"})
	sseWriteEvent(w, "message_stop", string(msgStop))
}

// writeAnthropicSSEToolUse writes an Anthropic streaming response that contains
// an optional text block followed by one or more tool_use blocks.
func writeAnthropicSSEToolUse(w http.ResponseWriter, id, model string, textContent string, toolBlocks []map[string]string, inputTok, outputTok int) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)

	msgStart, _ := json.Marshal(map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id": id, "type": "message", "role": "assistant", "model": model,
			"content": []any{}, "stop_reason": nil,
			"usage": map[string]int{"input_tokens": inputTok, "output_tokens": 0},
		},
	})
	sseWriteEvent(w, "message_start", string(msgStart))

	blockIdx := 0

	if textContent != "" {
		cbStart, _ := json.Marshal(map[string]any{
			"type": "content_block_start", "index": blockIdx,
			"content_block": map[string]string{"type": "text", "text": ""},
		})
		sseWriteEvent(w, "content_block_start", string(cbStart))

		cbDelta, _ := json.Marshal(map[string]any{
			"type": "content_block_delta", "index": blockIdx,
			"delta": map[string]string{"type": "text_delta", "text": textContent},
		})
		sseWriteEvent(w, "content_block_delta", string(cbDelta))

		cbStop, _ := json.Marshal(map[string]any{"type": "content_block_stop", "index": blockIdx})
		sseWriteEvent(w, "content_block_stop", string(cbStop))
		blockIdx++
	}

	for _, tb := range toolBlocks {
		cbStart, _ := json.Marshal(map[string]any{
			"type": "content_block_start", "index": blockIdx,
			"content_block": map[string]any{
				"type": "tool_use", "id": tb["id"], "name": tb["name"], "input": map[string]any{},
			},
		})
		sseWriteEvent(w, "content_block_start", string(cbStart))

		cbDelta, _ := json.Marshal(map[string]any{
			"type": "content_block_delta", "index": blockIdx,
			"delta": map[string]string{"type": "input_json_delta", "partial_json": tb["input"]},
		})
		sseWriteEvent(w, "content_block_delta", string(cbDelta))

		cbStop, _ := json.Marshal(map[string]any{"type": "content_block_stop", "index": blockIdx})
		sseWriteEvent(w, "content_block_stop", string(cbStop))
		blockIdx++
	}

	msgDelta, _ := json.Marshal(map[string]any{
		"type":  "message_delta",
		"delta": map[string]string{"stop_reason": "tool_use"},
		"usage": map[string]int{"output_tokens": outputTok},
	})
	sseWriteEvent(w, "message_delta", string(msgDelta))

	msgStop, _ := json.Marshal(map[string]any{"type": "message_stop"})
	sseWriteEvent(w, "message_stop", string(msgStop))
}

func TestGetEnv(t *testing.T) {
	tests := []struct {
		name     string
		key      string
		fallback string
		envVal   string
		want     string
	}{
		{"returns env value when set", "TEST_GET_ENV_1", "default", "custom", "custom"},
		{"returns fallback when unset", "TEST_GET_ENV_2", "default", "", "default"},
		{"returns empty string fallback", "TEST_GET_ENV_3", "", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.envVal != "" {
				t.Setenv(tt.key, tt.envVal)
			}
			got := getEnv(tt.key, tt.fallback)
			if got != tt.want {
				t.Errorf("getEnv(%q, %q) = %q, want %q", tt.key, tt.fallback, got, tt.want)
			}
		})
	}
}

func TestFirstNonEmpty(t *testing.T) {
	tests := []struct {
		name string
		vals []string
		want string
	}{
		{"returns first non-empty", []string{"", "a", "b"}, "a"},
		{"returns first when set", []string{"x", "y"}, "x"},
		{"returns empty when all empty", []string{"", "", ""}, ""},
		{"handles no args", nil, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := firstNonEmpty(tt.vals...)
			if got != tt.want {
				t.Errorf("firstNonEmpty(%v) = %q, want %q", tt.vals, got, tt.want)
			}
		})
	}
}

func TestTruncate(t *testing.T) {
	tests := []struct {
		name string
		s    string
		n    int
		want string
	}{
		{"short string unchanged", "hello", 10, "hello"},
		{"exact length unchanged", "hello", 5, "hello"},
		{"long string truncated", "hello world", 5, "hello..."},
		{"empty string", "", 5, ""},
		{"zero length", "hello", 0, "..."},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := truncate(tt.s, tt.n)
			if got != tt.want {
				t.Errorf("truncate(%q, %d) = %q, want %q", tt.s, tt.n, got, tt.want)
			}
		})
	}
}

func TestWriteJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "test.json")

	data := map[string]string{"key": "value"}
	writeJSON(path, data)

	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("failed to read written file: %v", err)
	}

	var got map[string]string
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("failed to unmarshal: %v", err)
	}
	if got["key"] != "value" {
		t.Errorf("got key=%q, want %q", got["key"], "value")
	}
}

func TestWriteJSON_CreatesSubdirectories(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a", "b", "c", "out.json")

	writeJSON(path, agentResult{Status: "success"})

	if _, err := os.Stat(path); os.IsNotExist(err) {
		t.Error("expected file to be created through nested directories")
	}
}

func TestAgentResultJSON(t *testing.T) {
	res := agentResult{
		Status:   "success",
		Response: "hello world",
	}
	res.Metrics.DurationMs = 1234
	res.Metrics.InputTokens = 10
	res.Metrics.OutputTokens = 20

	b, err := json.Marshal(res)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var got agentResult
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Status != "success" {
		t.Errorf("status = %q, want %q", got.Status, "success")
	}
	if got.Response != "hello world" {
		t.Errorf("response = %q, want %q", got.Response, "hello world")
	}
	if got.Metrics.DurationMs != 1234 {
		t.Errorf("durationMs = %d, want 1234", got.Metrics.DurationMs)
	}
	if got.Metrics.InputTokens != 10 || got.Metrics.OutputTokens != 20 {
		t.Errorf("tokens = (%d, %d), want (10, 20)", got.Metrics.InputTokens, got.Metrics.OutputTokens)
	}
}

func TestAgentResult_ErrorOmitsResponse(t *testing.T) {
	res := agentResult{
		Status: "error",
		Error:  "something broke",
	}

	b, err := json.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}

	var raw map[string]any
	json.Unmarshal(b, &raw)
	if _, ok := raw["response"]; ok {
		t.Error("expected response field to be omitted on error result")
	}
}

func TestStreamChunkJSON(t *testing.T) {
	chunk := streamChunk{Type: "text", Content: "hello", Index: 0}
	b, err := json.Marshal(chunk)
	if err != nil {
		t.Fatal(err)
	}
	var got streamChunk
	json.Unmarshal(b, &got)
	if got.Type != "text" || got.Content != "hello" || got.Index != 0 {
		t.Errorf("chunk roundtrip failed: %+v", got)
	}
}

func TestCallOpenAI_MockServer(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Errorf("unexpected path: %s", r.URL.Path)
			http.Error(w, "not found", 404)
			return
		}
		if r.Header.Get("Authorization") != "Bearer test-key" {
			t.Errorf("unexpected auth header: %s", r.Header.Get("Authorization"))
		}
		writeOpenAISSE(w, "chatcmpl-test", "gpt-4o-mini", "Hello from mock!", "stop", 5, 10)
	})

	srv := httptest.NewServer(handler)
	defer srv.Close()

	ctx := t.Context()
	text, inTok, outTok, _, err := callOpenAI(ctx, "openai", "test-key", srv.URL, "gpt-4o-mini", "You are helpful.", "Say hello", nil)
	if err != nil {
		t.Fatalf("callOpenAI error: %v", err)
	}
	if text != "Hello from mock!" {
		t.Errorf("text = %q, want %q", text, "Hello from mock!")
	}
	if inTok != 5 {
		t.Errorf("input tokens = %d, want 5", inTok)
	}
	if outTok != 10 {
		t.Errorf("output tokens = %d, want 10", outTok)
	}
}

func TestCallOpenAI_ServerError(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]string{
				"message": "invalid api key",
				"type":    "invalid_request_error",
				"code":    "invalid_api_key",
			},
		})
	})

	srv := httptest.NewServer(handler)
	defer srv.Close()

	ctx := t.Context()
	_, _, _, _, err := callOpenAI(ctx, "openai", "bad-key", srv.URL, "gpt-4", "sys", "task", nil)
	if err == nil {
		t.Fatal("expected error for 401 response")
	}
	if !strings.Contains(err.Error(), "401") && !strings.Contains(err.Error(), "API error") {
		t.Errorf("error should mention 401 or API error, got: %v", err)
	}
}

func TestCallAnthropic_MockServer(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			t.Errorf("unexpected path: %s (expected /v1/messages)", r.URL.Path)
			http.Error(w, "not found", 404)
			return
		}
		if r.Header.Get("X-Api-Key") != "test-anthropic-key" {
			t.Errorf("unexpected x-api-key header: %s", r.Header.Get("X-Api-Key"))
		}
		writeAnthropicSSEText(w, "msg_test", "claude-sonnet-4-20250514", "Hello from Anthropic mock!", "end_turn", 8, 12)
	})

	srv := httptest.NewServer(handler)
	defer srv.Close()

	ctx := t.Context()
	text, inTok, outTok, _, err := callAnthropic(ctx, "test-anthropic-key", srv.URL, "claude-sonnet-4-20250514", "Be helpful.", "Say hello", nil)
	if err != nil {
		t.Fatalf("callAnthropic error: %v", err)
	}
	if text != "Hello from Anthropic mock!" {
		t.Errorf("text = %q, want %q", text, "Hello from Anthropic mock!")
	}
	if inTok != 8 {
		t.Errorf("input tokens = %d, want 8", inTok)
	}
	if outTok != 12 {
		t.Errorf("output tokens = %d, want 12", outTok)
	}
}

func TestCallAnthropic_ServerError(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]any{
			"type": "error",
			"error": map[string]string{
				"type":    "invalid_request_error",
				"message": "Your credit balance is too low",
			},
		})
	})

	srv := httptest.NewServer(handler)
	defer srv.Close()

	ctx := t.Context()
	_, _, _, _, err := callAnthropic(ctx, "bad-key", srv.URL, "claude-sonnet-4-20250514", "sys", "task", nil)
	if err == nil {
		t.Fatal("expected error for 400 response")
	}
	if !strings.Contains(err.Error(), "400") && !strings.Contains(err.Error(), "API error") {
		t.Errorf("error should mention 400 or API error, got: %v", err)
	}
}

func TestCallOpenAI_AzureRequiresBaseURL(t *testing.T) {
	ctx := t.Context()
	_, _, _, _, err := callOpenAI(ctx, "azure-openai", "key", "", "gpt-4", "sys", "task", nil)
	if err == nil {
		t.Fatal("expected error when azure-openai has no base URL")
	}
	if !strings.Contains(err.Error(), "requires MODEL_BASE_URL") {
		t.Errorf("error = %v, want mention of MODEL_BASE_URL", err)
	}
}

func TestProviderRouting(t *testing.T) {
	openAICalled := false
	anthropicCalled := false

	openaiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		openAICalled = true
		writeOpenAISSE(w, "test", "m", "ok", "stop", 1, 1)
	}))
	defer openaiSrv.Close()

	anthropicSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		anthropicCalled = true
		writeAnthropicSSEText(w, "msg_test", "m", "ok", "end_turn", 1, 1)
	}))
	defer anthropicSrv.Close()

	ctx := t.Context()

	callOpenAI(ctx, "openai", "k", openaiSrv.URL, "m", "s", "t", nil)
	if !openAICalled {
		t.Error("expected OpenAI server to be called for openai provider")
	}

	callAnthropic(ctx, "k", anthropicSrv.URL, "m", "s", "t", nil)
	if !anthropicCalled {
		t.Error("expected Anthropic server to be called for anthropic provider")
	}

	// Verify Bedrock routing uses the mock client interface.
	bedrockCalled := false
	mockClient := &mockBedrockClient{
		handler: func(ctx context.Context, input *bedrockruntime.ConverseInput) (*bedrockruntime.ConverseOutput, error) {
			bedrockCalled = true
			return &bedrockruntime.ConverseOutput{
				Output: &types.ConverseOutputMemberMessage{
					Value: types.Message{
						Role:    types.ConversationRoleAssistant,
						Content: []types.ContentBlock{&types.ContentBlockMemberText{Value: "ok"}},
					},
				},
				StopReason: types.StopReasonEndTurn,
				Usage:      &types.TokenUsage{InputTokens: aws.Int32(1), OutputTokens: aws.Int32(1)},
			}, nil
		},
	}
	callBedrockWithClient(ctx, mockClient, "m", "s", "t", nil)
	if !bedrockCalled {
		t.Error("expected Bedrock mock client to be called")
	}
}

func TestCallAnthropic_ToolUseFlow(t *testing.T) {
	callCount := 0

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++

		if callCount == 1 {
			inputJSON, _ := json.Marshal(map[string]string{"path": "/tmp/testfile.txt"})
			writeAnthropicSSEToolUse(w, "msg_tool", "claude-sonnet-4-20250514",
				"I'll read that file for you.",
				[]map[string]string{{"id": "toolu_01ABC", "name": "read_file", "input": string(inputJSON)}},
				20, 30)
			return
		}

		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		messages, _ := body["messages"].([]any)
		if len(messages) < 3 {
			t.Errorf("expected at least 3 messages (user + assistant + tool_result), got %d", len(messages))
		}

		writeAnthropicSSEText(w, "msg_final", "claude-sonnet-4-20250514", "The file contains: hello world", "end_turn", 50, 15)
	})

	srv := httptest.NewServer(handler)
	defer srv.Close()

	// Create a temp file for the read_file tool to read.
	tmpDir := t.TempDir()
	tmpFile := filepath.Join(tmpDir, "testfile.txt")
	os.WriteFile(tmpFile, []byte("hello world"), 0o644)

	// Define a minimal read_file tool.
	tools := []ToolDef{
		{
			Name:        "read_file",
			Description: "Read a file",
			Parameters: map[string]any{
				"properties": map[string]any{
					"path": map[string]any{"type": "string", "description": "File path"},
				},
				"required": []string{"path"},
			},
		},
	}

	ctx := t.Context()
	text, inTok, outTok, toolCalls, err := callAnthropic(ctx, "key", srv.URL, "claude-sonnet-4-20250514", "sys", "Read /tmp/testfile.txt", tools)
	if err != nil {
		t.Fatalf("callAnthropic tool-use error: %v", err)
	}
	if callCount != 2 {
		t.Errorf("expected 2 API calls (tool_use + final), got %d", callCount)
	}
	if toolCalls != 1 {
		t.Errorf("expected 1 tool call, got %d", toolCalls)
	}
	if text != "The file contains: hello world" {
		t.Errorf("text = %q, want %q", text, "The file contains: hello world")
	}
	if inTok != 70 { // 20 + 50
		t.Errorf("input tokens = %d, want 70", inTok)
	}
	if outTok != 45 { // 30 + 15
		t.Errorf("output tokens = %d, want 45", outTok)
	}
}

func TestCallAnthropic_MultipleToolCalls(t *testing.T) {
	callCount := 0

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++

		if callCount == 1 {
			inputA, _ := json.Marshal(map[string]string{"path": "/workspace/a.txt"})
			inputB, _ := json.Marshal(map[string]string{"path": "/workspace/b.txt"})
			writeAnthropicSSEToolUse(w, "msg_multi", "claude-sonnet-4-20250514", "",
				[]map[string]string{
					{"id": "toolu_01A", "name": "read_file", "input": string(inputA)},
					{"id": "toolu_01B", "name": "read_file", "input": string(inputB)},
				}, 10, 20)
			return
		}

		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		messages, _ := body["messages"].([]any)
		lastMsg, _ := messages[len(messages)-1].(map[string]any)
		content, _ := lastMsg["content"].([]any)
		resultCount := 0
		for _, c := range content {
			block, _ := c.(map[string]any)
			if block["type"] == "tool_result" {
				resultCount++
			}
		}
		if resultCount != 2 {
			t.Errorf("expected 2 tool_result blocks, got %d", resultCount)
		}

		writeAnthropicSSEText(w, "msg_done", "claude-sonnet-4-20250514", "Both files read.", "end_turn", 30, 5)
	})

	srv := httptest.NewServer(handler)
	defer srv.Close()

	tools := []ToolDef{
		{
			Name:        "read_file",
			Description: "Read a file",
			Parameters: map[string]any{
				"properties": map[string]any{
					"path": map[string]any{"type": "string", "description": "File path"},
				},
				"required": []string{"path"},
			},
		},
	}

	ctx := t.Context()
	text, _, _, toolCalls, err := callAnthropic(ctx, "key", srv.URL, "claude-sonnet-4-20250514", "sys", "Read both", tools)
	if err != nil {
		t.Fatalf("callAnthropic multi-tool error: %v", err)
	}
	if toolCalls != 2 {
		t.Errorf("expected 2 tool calls, got %d", toolCalls)
	}
	if text != "Both files read." {
		t.Errorf("text = %q, want %q", text, "Both files read.")
	}
}

func TestCallAnthropic_ToolErrorIsError(t *testing.T) {
	callCount := 0
	var capturedBody map[string]any

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++

		if callCount == 1 {
			inputJSON, _ := json.Marshal(map[string]string{"path": "/nonexistent/file.txt"})
			writeAnthropicSSEToolUse(w, "msg_err", "claude-sonnet-4-20250514", "",
				[]map[string]string{{"id": "toolu_err", "name": "read_file", "input": string(inputJSON)}},
				5, 10)
			return
		}

		json.NewDecoder(r.Body).Decode(&capturedBody)
		writeAnthropicSSEText(w, "msg_recovery", "claude-sonnet-4-20250514", "The file was not found.", "end_turn", 15, 8)
	})

	srv := httptest.NewServer(handler)
	defer srv.Close()

	tools := []ToolDef{
		{
			Name:        "read_file",
			Description: "Read a file",
			Parameters: map[string]any{
				"properties": map[string]any{
					"path": map[string]any{"type": "string", "description": "File path"},
				},
				"required": []string{"path"},
			},
		},
	}

	ctx := t.Context()
	text, _, _, _, err := callAnthropic(ctx, "key", srv.URL, "claude-sonnet-4-20250514", "sys", "Read /nonexistent/file.txt", tools)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if text != "The file was not found." {
		t.Errorf("text = %q, want %q", text, "The file was not found.")
	}

	// Verify the is_error field was set on the tool_result.
	messages, _ := capturedBody["messages"].([]any)
	lastMsg, _ := messages[len(messages)-1].(map[string]any)
	content, _ := lastMsg["content"].([]any)
	for _, c := range content {
		block, _ := c.(map[string]any)
		if block["type"] == "tool_result" {
			isErr, _ := block["is_error"].(bool)
			if !isErr {
				t.Error("expected is_error=true for tool result starting with 'Error:'")
			}
		}
	}
}
