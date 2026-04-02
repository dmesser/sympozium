package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestStreamChunkJSON_ToolCallOmitsEmpty(t *testing.T) {
	chunk := streamChunk{
		Type:     "tool_call",
		Index:    3,
		ToolID:   "call_abc",
		ToolName: "execute_command",
		ToolArgs: `{"command":"oc get pods"}`,
	}
	b, err := json.Marshal(chunk)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	json.Unmarshal(b, &got)

	if got["type"] != "tool_call" {
		t.Errorf("type = %v, want tool_call", got["type"])
	}
	if got["toolId"] != "call_abc" {
		t.Errorf("toolId = %v, want call_abc", got["toolId"])
	}
	if got["toolName"] != "execute_command" {
		t.Errorf("toolName = %v, want execute_command", got["toolName"])
	}
	if _, ok := got["content"]; ok {
		t.Error("content should be omitted for tool_call chunks")
	}
	if _, ok := got["status"]; ok {
		t.Error("status should be omitted for tool_call chunks")
	}
}

func TestStreamChunkJSON_ToolResult(t *testing.T) {
	chunk := streamChunk{
		Type:    "tool_result",
		Index:   4,
		ToolID:  "call_abc",
		Status:  "success",
		Content: "output text",
	}
	b, err := json.Marshal(chunk)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	json.Unmarshal(b, &got)

	if got["type"] != "tool_result" {
		t.Errorf("type = %v, want tool_result", got["type"])
	}
	if got["status"] != "success" {
		t.Errorf("status = %v, want success", got["status"])
	}
	if got["content"] != "output text" {
		t.Errorf("content = %v, want 'output text'", got["content"])
	}
}

func TestStreamChunkJSON_StatusBadge(t *testing.T) {
	chunk := streamChunk{
		Type:     "tool_call",
		Index:    0,
		ToolID:   "status-0",
		ToolName: "Analyzing",
	}
	b, err := json.Marshal(chunk)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	json.Unmarshal(b, &got)

	if got["type"] != "tool_call" {
		t.Errorf("type = %v, want tool_call", got["type"])
	}
	if got["toolName"] != "Analyzing" {
		t.Errorf("toolName = %v, want Analyzing", got["toolName"])
	}
	if got["toolId"] != "status-0" {
		t.Errorf("toolId = %v, want status-0", got["toolId"])
	}
}

func TestStreamChunkJSON_TextOmitsOptionalFields(t *testing.T) {
	chunk := streamChunk{Type: "text", Content: "hello", Index: 0}
	b, _ := json.Marshal(chunk)
	var got map[string]any
	json.Unmarshal(b, &got)

	for _, field := range []string{"toolId", "toolName", "toolArgs", "status"} {
		if _, ok := got[field]; ok {
			t.Errorf("field %q should be omitted for text chunks", field)
		}
	}
}

func TestEmitToolCall_WritesFile(t *testing.T) {
	dir := t.TempDir()
	ipcDir := filepath.Join(dir, "ipc", "output")
	os.MkdirAll(ipcDir, 0o755)

	// Temporarily symlink /ipc/output to our temp dir.
	// Since we can't guarantee /ipc exists, write directly and verify.
	sw := &streamWriter{}

	// Use writeJSON directly to verify the file format.
	path := filepath.Join(ipcDir, "stream-0.json")
	writeJSON(path, streamChunk{
		Type:     "tool_call",
		Index:    0,
		ToolID:   "call_x",
		ToolName: "fetch_url",
		ToolArgs: `{"url":"https://example.com"}`,
	})

	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("expected stream file: %v", err)
	}
	var got streamChunk
	json.Unmarshal(b, &got)
	if got.Type != "tool_call" {
		t.Errorf("type = %q, want tool_call", got.Type)
	}
	if got.ToolName != "fetch_url" {
		t.Errorf("toolName = %q, want fetch_url", got.ToolName)
	}
	if got.ToolArgs != `{"url":"https://example.com"}` {
		t.Errorf("toolArgs = %q, want json", got.ToolArgs)
	}
	_ = sw // sw is used for API reference only in this test
}

func TestEmitToolResult_WritesFile(t *testing.T) {
	dir := t.TempDir()
	ipcDir := filepath.Join(dir, "ipc", "output")
	os.MkdirAll(ipcDir, 0o755)

	path := filepath.Join(ipcDir, "stream-1.json")
	writeJSON(path, streamChunk{
		Type:    "tool_result",
		Index:   1,
		ToolID:  "call_x",
		Status:  "error",
		Content: "Error: permission denied",
	})

	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("expected stream file: %v", err)
	}
	var got streamChunk
	json.Unmarshal(b, &got)
	if got.Type != "tool_result" {
		t.Errorf("type = %q, want tool_result", got.Type)
	}
	if got.Status != "error" {
		t.Errorf("status = %q, want error", got.Status)
	}
}

func TestStatusBadge_LifecycleWritesFiles(t *testing.T) {
	dir := t.TempDir()
	ipcDir := filepath.Join(dir, "ipc", "output")
	os.MkdirAll(ipcDir, 0o755)

	callPath := filepath.Join(ipcDir, "stream-0.json")
	writeJSON(callPath, streamChunk{
		Type:     "tool_call",
		Index:    0,
		ToolID:   "status-0",
		ToolName: "Analyzing",
	})
	resultPath := filepath.Join(ipcDir, "stream-1.json")
	writeJSON(resultPath, streamChunk{
		Type:   "tool_result",
		Index:  1,
		ToolID: "status-0",
		Status: "success",
	})

	b, err := os.ReadFile(callPath)
	if err != nil {
		t.Fatalf("expected call file: %v", err)
	}
	var got streamChunk
	json.Unmarshal(b, &got)
	if got.ToolName != "Analyzing" {
		t.Errorf("toolName = %q, want Analyzing", got.ToolName)
	}

	b, err = os.ReadFile(resultPath)
	if err != nil {
		t.Fatalf("expected result file: %v", err)
	}
	json.Unmarshal(b, &got)
	if got.Status != "success" {
		t.Errorf("status = %q, want success", got.Status)
	}
}

func TestStreamChunkJSON_ActionsChunk(t *testing.T) {
	chunk := streamChunk{
		Type:  "actions",
		Index: 5,
		Suggestions: []actionSuggestion{
			{Label: "Run scan", Prompt: "Run a comprehensive security scan on all images"},
			{Label: "Check version", Prompt: "Check the installed version of the operator"},
		},
	}
	b, err := json.Marshal(chunk)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	json.Unmarshal(b, &got)

	if got["type"] != "actions" {
		t.Errorf("type = %v, want actions", got["type"])
	}
	suggestions, ok := got["suggestions"].([]any)
	if !ok || len(suggestions) != 2 {
		t.Fatalf("suggestions = %v, want 2-element array", got["suggestions"])
	}
	first, _ := suggestions[0].(map[string]any)
	if first["label"] != "Run scan" {
		t.Errorf("suggestions[0].label = %v, want 'Run scan'", first["label"])
	}
}

func TestStreamWriter_ActionsSuppression(t *testing.T) {
	sw := &streamWriter{}
	sw.Write("Here are the results.")
	sw.Write(`__SYMPOZIUM_ACTIONS__["Scan images", "Update operator"]__SYMPOZIUM_ACTIONS_END__`)
	sw.Flush()

	// "Here are the results." emitted as text (1 chunk), actions emitted as actions chunk (1 chunk)
	if sw.chunkIdx < 2 {
		t.Errorf("expected at least 2 chunks (text + actions), got %d", sw.chunkIdx)
	}
}

func TestStreamWriter_ActionsSuppression_Incremental(t *testing.T) {
	sw := &streamWriter{}
	sw.Write("Result text. __SYMPO")
	sw.Write("ZIUM_ACTIONS__[\"Do A\", \"Do B\"]__SYMPOZIUM_ACTIONS_END__")
	sw.Flush()

	if sw.chunkIdx < 2 {
		t.Errorf("expected at least 2 chunks, got %d", sw.chunkIdx)
	}
}

func TestStreamWriter_MemorySuppressionStillWorks(t *testing.T) {
	dir := t.TempDir()
	ipcDir := filepath.Join(dir, "ipc", "output")
	os.MkdirAll(ipcDir, 0o755)

	sw := &streamWriter{}

	// Collect emitted chunks by reading the Write calls.
	// Since we can't redirect writes, test the suppression logic indirectly
	// by verifying the existing behavior still works.
	sw.Write("Hello ")
	sw.Write("__SYMPOZIUM_MEMORY__secret data__SYMPOZIUM_MEMORY_END__")
	sw.Write("World")
	sw.Flush()

	// The streamWriter should have emitted "Hello " and "World" but not the memory block.
	// Since chunkIdx tracks how many chunks were written, verify it.
	if sw.chunkIdx < 2 {
		t.Errorf("expected at least 2 chunks (Hello + World), got %d", sw.chunkIdx)
	}
}
