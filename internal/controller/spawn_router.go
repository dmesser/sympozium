package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/go-logr/logr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	sympoziumv1alpha1 "github.com/sympozium-ai/sympozium/api/v1alpha1"
	"github.com/sympozium-ai/sympozium/internal/eventbus"
)

// SpawnRouter subscribes to agent.spawn.request on the event bus and creates
// child AgentRun CRs. When the child completes, the result is forwarded back
// so the parent agent can resume.
type SpawnRouter struct {
	Client   client.Client
	EventBus eventbus.EventBus
	Log      logr.Logger
}

type spawnRequestPayload struct {
	Task         string   `json:"task"`
	SystemPrompt string   `json:"systemPrompt,omitempty"`
	AgentID      string   `json:"agentId"`
	Skills       []string `json:"skills,omitempty"`
}

// Start begins listening for spawn requests and completed runs.
func (sr *SpawnRouter) Start(ctx context.Context) error {
	sr.Log.Info("Starting spawn router")

	spawnCh, err := sr.EventBus.Subscribe(ctx, eventbus.TopicAgentSpawnRequest)
	if err != nil {
		return fmt.Errorf("subscribing to %s: %w", eventbus.TopicAgentSpawnRequest, err)
	}

	completedCh, err := sr.EventBus.Subscribe(ctx, eventbus.TopicAgentRunCompleted)
	if err != nil {
		return fmt.Errorf("subscribing to %s: %w", eventbus.TopicAgentRunCompleted, err)
	}

	for {
		select {
		case <-ctx.Done():
			sr.Log.Info("Spawn router shutting down")
			return nil
		case event := <-spawnCh:
			sr.handleSpawnRequest(ctx, event)
		case event := <-completedCh:
			sr.handleChildCompleted(ctx, event)
		}
	}
}

func (sr *SpawnRouter) handleSpawnRequest(ctx context.Context, event *eventbus.Event) {
	parentRunID := event.Metadata["agentRunID"]
	instanceName := event.Metadata["instanceName"]

	var req spawnRequestPayload
	if err := json.Unmarshal(event.Data, &req); err != nil {
		sr.Log.Error(err, "failed to unmarshal spawn request")
		return
	}

	if req.Task == "" || instanceName == "" {
		sr.Log.Info("Skipping spawn request with missing task or instance",
			"instance", instanceName, "agentId", req.AgentID)
		return
	}

	// Look up the parent instance for model config.
	var instances sympoziumv1alpha1.SympoziumInstanceList
	if err := sr.Client.List(ctx, &instances); err != nil {
		sr.Log.Error(err, "failed to list SympoziumInstances")
		return
	}

	var inst *sympoziumv1alpha1.SympoziumInstance
	for i := range instances.Items {
		if instances.Items[i].Name == instanceName {
			inst = &instances.Items[i]
			break
		}
	}
	if inst == nil {
		sr.Log.Info("SympoziumInstance not found for spawn request", "instance", instanceName)
		return
	}

	// Build skill refs from the request.
	var skillRefs []sympoziumv1alpha1.SkillRef
	for _, s := range req.Skills {
		skillRefs = append(skillRefs, sympoziumv1alpha1.SkillRef{SkillPackRef: s})
	}

	provider := resolveProvider(inst)
	authSecret := resolveAuthSecret(inst)

	run := &sympoziumv1alpha1.AgentRun{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: fmt.Sprintf("%s-sub-", instanceName),
			Namespace:    inst.Namespace,
			Labels: map[string]string{
				"sympozium.ai/instance":   instanceName,
				"sympozium.ai/agent-id":   req.AgentID,
				"sympozium.ai/parent-run": parentRunID,
				"sympozium.ai/component":  "agent-run",
				"sympozium.ai/source":     "spawn",
			},
		},
		Spec: sympoziumv1alpha1.AgentRunSpec{
			InstanceRef: instanceName,
			AgentID:     req.AgentID,
			SessionKey:  fmt.Sprintf("spawn-%s-%s-%d", parentRunID, req.AgentID, time.Now().UnixNano()),
			Parent: &sympoziumv1alpha1.ParentRunRef{
				RunName:    parentRunID,
				SessionKey: fmt.Sprintf("parent-%s", parentRunID),
				SpawnDepth: 1,
			},
			Task:         req.Task,
			SystemPrompt: req.SystemPrompt,
			Model: sympoziumv1alpha1.ModelSpec{
				Provider:      provider,
				Model:         inst.Spec.Agents.Default.Model,
				BaseURL:       inst.Spec.Agents.Default.BaseURL,
				AuthSecretRef: authSecret,
				NodeSelector:  inst.Spec.Agents.Default.NodeSelector,
			},
			Skills:           append(inst.Spec.Skills, skillRefs...),
			Timeout:          &metav1.Duration{Duration: 5 * time.Minute},
			Cleanup:          "delete",
			ImagePullSecrets: inst.Spec.ImagePullSecrets,
		},
	}

	if err := sr.Client.Create(ctx, run); err != nil {
		sr.Log.Error(err, "failed to create child AgentRun from spawn request",
			"parent", parentRunID, "agentId", req.AgentID)
		return
	}

	sr.Log.Info("Created child AgentRun from spawn request",
		"child", run.Name, "parent", parentRunID, "skill", req.AgentID)
}

// handleChildCompleted checks if a completed run was spawned by a parent
// and forwards the result back via the event bus so the parent's IPC bridge
// can write a result file for the polling spawn_subagent tool.
func (sr *SpawnRouter) handleChildCompleted(ctx context.Context, event *eventbus.Event) {
	agentRunID := event.Metadata["agentRunID"]
	if agentRunID == "" {
		return
	}

	var runs sympoziumv1alpha1.AgentRunList
	if err := sr.Client.List(ctx, &runs, client.MatchingLabels{
		"sympozium.ai/source": "spawn",
	}); err != nil {
		sr.Log.Error(err, "failed to list spawn-sourced AgentRuns")
		return
	}

	var run *sympoziumv1alpha1.AgentRun
	for i := range runs.Items {
		if runs.Items[i].Name == agentRunID {
			run = &runs.Items[i]
			break
		}
	}
	if run == nil || run.Spec.Parent == nil {
		return
	}

	parentRunID := run.Labels["sympozium.ai/parent-run"]
	if parentRunID == "" {
		return
	}

	// Publish the child result on a per-parent topic so the parent's IPC
	// bridge can relay it back as a spawn result file.
	topic := fmt.Sprintf("agent.spawn.result.%s", parentRunID)
	resultEvent, err := eventbus.NewEvent(topic, map[string]string{
		"parentRunID":  parentRunID,
		"childRunID":   agentRunID,
		"instanceName": run.Labels["sympozium.ai/instance"],
		"agentId":      strings.TrimPrefix(run.Labels["sympozium.ai/agent-id"], ""),
	}, json.RawMessage(event.Data))
	if err != nil {
		sr.Log.Error(err, "failed to create spawn result event")
		return
	}

	if err := sr.EventBus.Publish(ctx, topic, resultEvent); err != nil {
		sr.Log.Error(err, "failed to publish spawn result to parent",
			"parent", parentRunID, "child", agentRunID)
		return
	}

	sr.Log.Info("Forwarded child result to parent",
		"child", agentRunID, "parent", parentRunID)
}
