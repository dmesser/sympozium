package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/go-logr/logr"

	"github.com/sympozium-ai/sympozium/internal/eventbus"
	"github.com/sympozium-ai/sympozium/internal/ipc"
)

// ApprovalRouter subscribes to tool.approval.request on the event bus,
// tracks pending approvals, and relays decisions back to the originating
// agent's IPC bridge via tool.approval.response.{agentRunID}.
//
// External consumers (e.g. OLS web-proxy, REST API) call SubmitDecision
// to approve or reject a pending tool execution.
type ApprovalRouter struct {
	EventBus eventbus.EventBus
	Log      logr.Logger

	mu       sync.Mutex
	pending  map[string]*pendingApproval // keyed by approval request ID
}

type pendingApproval struct {
	AgentRunID   string
	InstanceName string
	Request      ipc.ApprovalRequest
}

// Start begins listening for tool approval requests.
func (ar *ApprovalRouter) Start(ctx context.Context) error {
	ar.Log.Info("Starting approval router")
	ar.pending = make(map[string]*pendingApproval)

	approvalCh, err := ar.EventBus.Subscribe(ctx, eventbus.TopicToolApprovalRequest)
	if err != nil {
		return fmt.Errorf("subscribing to %s: %w", eventbus.TopicToolApprovalRequest, err)
	}

	for {
		select {
		case <-ctx.Done():
			ar.Log.Info("Approval router shutting down")
			return nil
		case event := <-approvalCh:
			ar.handleApprovalRequest(ctx, event)
		}
	}
}

func (ar *ApprovalRouter) handleApprovalRequest(ctx context.Context, event *eventbus.Event) {
	agentRunID := event.Metadata["agentRunID"]
	instanceName := event.Metadata["instanceName"]

	var req ipc.ApprovalRequest
	if err := json.Unmarshal(event.Data, &req); err != nil {
		ar.Log.Error(err, "failed to unmarshal approval request")
		return
	}

	ar.mu.Lock()
	ar.pending[req.ID] = &pendingApproval{
		AgentRunID:   agentRunID,
		InstanceName: instanceName,
		Request:      req,
	}
	ar.mu.Unlock()

	ar.Log.Info("Registered pending tool approval",
		"approvalId", req.ID,
		"tool", req.ToolName,
		"agentRunID", agentRunID,
	)

	// Publish to a web-facing topic so external consumers (OLS streaming,
	// web-proxy REST) can pick it up and present it to the user.
	webTopic := fmt.Sprintf("tool.approval.pending.%s", instanceName)
	webEvent, _ := eventbus.NewEvent(webTopic, event.Metadata, event.Data)
	if err := ar.EventBus.Publish(ctx, webTopic, webEvent); err != nil {
		ar.Log.Error(err, "failed to publish pending approval to web topic")
	}
}

// SubmitDecision resolves a pending approval. Called by external consumers
// (REST API, web-proxy) after the user approves or rejects.
func (ar *ApprovalRouter) SubmitDecision(ctx context.Context, approvalID string, approved bool, message string) error {
	ar.mu.Lock()
	p, ok := ar.pending[approvalID]
	if ok {
		delete(ar.pending, approvalID)
	}
	ar.mu.Unlock()

	if !ok {
		return fmt.Errorf("no pending approval with id %q", approvalID)
	}

	resp := ipc.ApprovalResponse{
		ID:       approvalID,
		Approved: approved,
		Message:  message,
	}

	data, err := json.Marshal(resp)
	if err != nil {
		return fmt.Errorf("marshalling approval response: %w", err)
	}

	topic := fmt.Sprintf("tool.approval.response.%s", p.AgentRunID)
	event, _ := eventbus.NewEvent(topic, map[string]string{
		"agentRunID":   p.AgentRunID,
		"instanceName": p.InstanceName,
		"approvalId":   approvalID,
	}, json.RawMessage(data))

	if err := ar.EventBus.Publish(ctx, topic, event); err != nil {
		return fmt.Errorf("publishing approval response: %w", err)
	}

	ar.Log.Info("Submitted approval decision",
		"approvalId", approvalID,
		"tool", p.Request.ToolName,
		"approved", approved,
	)
	return nil
}

// ListPending returns all currently pending approval requests.
func (ar *ApprovalRouter) ListPending() []ipc.ApprovalRequest {
	ar.mu.Lock()
	defer ar.mu.Unlock()

	result := make([]ipc.ApprovalRequest, 0, len(ar.pending))
	for _, p := range ar.pending {
		result = append(result, p.Request)
	}
	return result
}
