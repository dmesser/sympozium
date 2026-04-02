package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// approvalRequest is written to /ipc/tools/approval-request-{id}.json
// for the IPC bridge to forward to the controller.
type approvalRequest struct {
	ID       string         `json:"id"`
	ToolName string         `json:"toolName"`
	ToolArgs map[string]any `json:"toolArgs"`
	Reason   string         `json:"reason"`
}

// approvalResponse is written by the IPC bridge to /ipc/tools/approval-response-{id}.json
// after the controller receives a human decision.
type approvalResponse struct {
	ID       string `json:"id"`
	Approved bool   `json:"approved"`
	Message  string `json:"message,omitempty"`
}

var askTools map[string]bool

func initApprovalPolicy() {
	askTools = make(map[string]bool)
	raw := os.Getenv("TOOL_POLICY_ASK")
	if raw == "" {
		return
	}
	for _, t := range strings.Split(raw, ",") {
		t = strings.TrimSpace(t)
		if t != "" {
			askTools[t] = true
		}
	}
	if len(askTools) > 0 {
		log.Printf("approval policy: %d tool(s) require human approval: %s", len(askTools), raw)
	}
}

// requiresApproval returns true if the tool is gated by the ask policy.
func requiresApproval(toolName string) bool {
	return askTools[toolName]
}

// requestApproval writes an approval request via IPC and blocks until
// the human responds or the timeout expires.
// Returns (approved, error).
func requestApproval(toolName string, toolArgs map[string]any) (bool, error) {
	id := fmt.Sprintf("%d", time.Now().UnixNano())

	req := approvalRequest{
		ID:       id,
		ToolName: toolName,
		ToolArgs: toolArgs,
		Reason:   fmt.Sprintf("Tool '%s' requires human approval before execution (policy: ask)", toolName),
	}

	data, err := json.Marshal(req)
	if err != nil {
		return false, fmt.Errorf("marshalling approval request: %w", err)
	}

	toolsDir := "/ipc/tools"
	_ = os.MkdirAll(toolsDir, 0o755)

	reqPath := filepath.Join(toolsDir, fmt.Sprintf("approval-request-%s.json", id))
	if err := os.WriteFile(reqPath, data, 0o644); err != nil {
		return false, fmt.Errorf("writing approval request: %w", err)
	}

	log.Printf("Approval requested for tool '%s' (id=%s), waiting for human decision...", toolName, id)

	// Poll for the response from the IPC bridge.
	resPattern := fmt.Sprintf("approval-response-%s.json", id)
	deadline := time.Now().Add(5 * time.Minute)

	for time.Now().Before(deadline) {
		// Check for a direct response file matching our ID.
		resPath := filepath.Join(toolsDir, resPattern)
		resData, err := os.ReadFile(resPath)
		if err == nil && len(resData) > 0 {
			var resp approvalResponse
			if json.Unmarshal(resData, &resp) == nil {
				_ = os.Remove(reqPath)
				_ = os.Remove(resPath)
				log.Printf("Approval response for tool '%s': approved=%v", toolName, resp.Approved)
				return resp.Approved, nil
			}
		}

		// Also scan for any approval-response files that the bridge may have
		// written with a different timestamp-based name but containing our ID.
		entries, _ := os.ReadDir(toolsDir)
		for _, e := range entries {
			if !strings.HasPrefix(e.Name(), "approval-response-") {
				continue
			}
			path := filepath.Join(toolsDir, e.Name())
			d, err := os.ReadFile(path)
			if err != nil {
				continue
			}
			var resp approvalResponse
			if json.Unmarshal(d, &resp) == nil && resp.ID == id {
				_ = os.Remove(reqPath)
				_ = os.Remove(path)
				log.Printf("Approval response for tool '%s': approved=%v", toolName, resp.Approved)
				return resp.Approved, nil
			}
		}

		time.Sleep(2 * time.Second)
	}

	_ = os.Remove(reqPath)
	return false, fmt.Errorf("approval timed out after 5 minutes for tool '%s'", toolName)
}
