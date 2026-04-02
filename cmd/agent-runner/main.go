package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	anthropicoption "github.com/anthropics/anthropic-sdk-go/option"
	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/azure"
	openaioption "github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/shared"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	oteltrace "go.opentelemetry.io/otel/trace"
)

// maxToolIterations is the maximum number of tool-call round-trips before
// the agent stops and returns whatever text it has.
var maxToolIterations = 50

func init() {
	if val := os.Getenv("MAX_TOOL_ITERATIONS"); val != "" {
		if n, err := strconv.Atoi(val); err == nil && n > 0 {
			maxToolIterations = n
		}
	}
}

type agentResult struct {
	Status   string `json:"status"`
	Response string `json:"response,omitempty"`
	Error    string `json:"error,omitempty"`
	Metrics  struct {
		DurationMs   int64 `json:"durationMs"`
		InputTokens  int   `json:"inputTokens"`
		OutputTokens int   `json:"outputTokens"`
		ToolCalls    int   `json:"toolCalls"`
	} `json:"metrics"`
}

type actionSuggestion struct {
	Label  string `json:"label"`
	Prompt string `json:"prompt"`
}

type streamChunk struct {
	Type        string             `json:"type"`
	Content     string             `json:"content,omitempty"`
	Index       int                `json:"index"`
	ToolID      string             `json:"toolId,omitempty"`
	ToolName    string             `json:"toolName,omitempty"`
	ToolArgs    string             `json:"toolArgs,omitempty"`
	Status      string             `json:"status,omitempty"`
	Suggestions []actionSuggestion `json:"suggestions,omitempty"`
}

func main() {
	log.SetFlags(log.Ltime | log.Lmicroseconds)
	log.Println("agent-runner starting")

	initApprovalPolicy()

	task := getEnv("TASK", "")
	if task == "" {
		if b, err := os.ReadFile("/ipc/input/task.json"); err == nil {
			var input struct {
				Task string `json:"task"`
			}
			if json.Unmarshal(b, &input) == nil && input.Task != "" {
				task = input.Task
			}
		}
	}
	if task == "" {
		fatal("TASK env var is empty and no /ipc/input/task.json found")
	}

	systemPrompt := getEnv("SYSTEM_PROMPT", "You are a helpful AI assistant.")
	provider := strings.ToLower(getEnv("MODEL_PROVIDER", "openai"))
	modelName := getEnv("MODEL_NAME", "gpt-4o-mini")
	baseURL := strings.TrimRight(getEnv("MODEL_BASE_URL", ""), "/")
	memoryEnabled := getEnv("MEMORY_ENABLED", "") == "true"
	toolsEnabled := getEnv("TOOLS_ENABLED", "") == "true"

	// Load skill files and build enhanced system prompt.
	skills := loadSkills(defaultSkillsDir)
	systemPrompt = buildSystemPrompt(systemPrompt, skills, toolsEnabled)

	// If this run was triggered from a channel, inject context so the
	// agent knows how to reply through the originating channel.
	sourceChannel := getEnv("SOURCE_CHANNEL", "")
	sourceChatID := getEnv("SOURCE_CHAT_ID", "")
	if sourceChannel != "" {
		channelCtx := fmt.Sprintf(
			"\n\n## Channel Context\n\n"+
				"This task was received through the **%s** channel (chat ID: %s). "+
				"You can reply through this channel using the `send_channel_message` tool "+
				"with channel=%q and chatId=%q. Use it to deliver results, ask follow-up "+
				"questions, or send notifications to the user.",
			sourceChannel, sourceChatID, sourceChannel, sourceChatID,
		)
		systemPrompt += channelCtx
		log.Printf("channel context injected: channel=%s chatId=%s", sourceChannel, sourceChatID)
	}

	// Resolve tool definitions.
	var tools []ToolDef
	if toolsEnabled {
		tools = defaultTools()
		// Load MCP tools from manifest if the mcp-bridge sidecar is running
		if mcpTools := loadMCPTools("/ipc/tools/mcp-tools.json"); len(mcpTools) > 0 {
			tools = append(tools, mcpTools...)

			// Group tools by server prefix
			serverTools := make(map[string][]string)
			for _, t := range mcpTools {
				parts := strings.SplitN(t.Name, "_", 2)
				prefix := parts[0]
				serverTools[prefix] = append(serverTools[prefix], t.Name)
			}

			var sb strings.Builder
			sb.WriteString("\n\n## Specialized MCP Tools\n\n")
			sb.WriteString(fmt.Sprintf("You have access to %d specialized MCP tools. ", len(mcpTools)))
			sb.WriteString("ALWAYS prefer MCP tools over execute_command when a relevant MCP tool exists.\n\n")
			sb.WriteString("### Diagnostic Methodology\n")
			sb.WriteString("1. **Start targeted**: Use the most specific MCP tool for the problem first\n")
			sb.WriteString("2. **Don't shotgun**: Avoid calling many tools to 'gather info' — diagnose step by step\n")
			sb.WriteString("3. **Read results carefully**: Each MCP tool returns structured diagnostic data. Analyze it before calling more tools.\n")
			sb.WriteString("4. **gRPC != HTTP**: For gRPC issues, check port naming (grpc-*), appProtocol, H2 settings, DestinationRules — NOT path routing\n")
			sb.WriteString("5. **Only fall back to execute_command** for tasks no MCP tool covers (e.g., reading app logs)\n\n")

			sb.WriteString("### Available Tool Groups\n")
			for prefix, tools := range serverTools {
				sb.WriteString(fmt.Sprintf("- **%s** (%d tools): %s\n", prefix, len(tools), strings.Join(tools, ", ")))
			}

			systemPrompt += sb.String()
		}
		log.Printf("tools enabled: %d tool(s) registered", len(tools))
	}

	// Load memory tools if the memory server is available (standalone deployment).
	if memoryTools := initMemoryTools(); len(memoryTools) > 0 {
		tools = append(tools, memoryTools...)

		systemPrompt += "\n\n## Persistent Memory\n\n" +
			"You have access to persistent memory tools that survive across runs.\n" +
			"**Before starting any investigation**, call `memory_search` with relevant keywords " +
			"to check if similar issues have been diagnosed before.\n" +
			"**After completing your task**, call `memory_store` to save key findings, " +
			"root causes, and resolution steps for future reference.\n" +
			"Be specific in stored content — include service names, namespaces, error messages, and timestamps."

		log.Printf("memory tools loaded: %d tool(s)", len(memoryTools))
	} else if memoryEnabled {
		// Fallback: legacy ConfigMap memory for backward compatibility.
		var memoryContent string
		if b, err := os.ReadFile("/memory/MEMORY.md"); err == nil {
			memoryContent = strings.TrimSpace(string(b))
			log.Printf("loaded legacy memory (%d bytes)", len(memoryContent))
		}
		if memoryContent != "" && memoryContent != "# Agent Memory\n\nNo memories recorded yet." {
			task = fmt.Sprintf("## Your Memory\nThe following is your persistent memory from prior interactions:\n\n%s\n\n## Current Task\n%s", memoryContent, task)
		}
		if memoryEnabled {
			memoryInstruction := "\n\nYou have persistent memory. After completing your task, " +
				"output a memory update block wrapped in markers like this:\n" +
				"__SYMPOZIUM_MEMORY__\n<your updated MEMORY.md content>\n__SYMPOZIUM_MEMORY_END__\n" +
				"Include key facts, preferences, and context from this and past interactions. " +
				"Keep it concise (under 256KB). Use markdown format."
			systemPrompt += memoryInstruction
		}
	}

	systemPrompt += "\n\n## Suggested Actions (MANDATORY FORMAT)\n\n" +
		"IMPORTANT: NEVER write follow-up suggestions, next steps, or offers as inline text " +
		"(e.g. \"I can next...\", \"Would you like me to...\", \"If you want, I can...\", bullet lists of options). " +
		"The user interface renders suggestions as clickable buttons ONLY when you use the marker format below. " +
		"Inline text suggestions are NOT clickable and create a poor experience.\n\n" +
		"Instead, ALWAYS use this marker block at the very end of your response:\n" +
		"__SYMPOZIUM_ACTIONS__\n" +
		"[{\"label\":\"Check version\",\"prompt\":\"Check the currently installed version of the operator and compare it with the latest available version in the catalog\"}" +
		",{\"label\":\"Run security scan\",\"prompt\":\"Run a comprehensive CVE security scan on all container images used by the application in this namespace\"}]\n" +
		"__SYMPOZIUM_ACTIONS_END__\n\n" +
		"Rules:\n" +
		"- Labels: concise imperative phrases (2-6 words) for button text.\n" +
		"- Prompts: detailed enough for the agent to act on without ambiguity.\n" +
		"- Include 2-4 suggestions maximum.\n" +
		"- Omit the block entirely if no follow-up actions apply.\n" +
		"- End your visible response text BEFORE the marker block. Do NOT list the options in prose."

	apiKey := firstNonEmpty(
		os.Getenv("API_KEY"),
		os.Getenv("OPENAI_API_KEY"),
		os.Getenv("ANTHROPIC_API_KEY"),
		os.Getenv("AZURE_OPENAI_API_KEY"),
		os.Getenv("PROVIDER_API_KEY"),
	)

	log.Printf("provider=%s model=%s baseURL=%s tools=%v task=%q",
		provider, modelName, baseURL, toolsEnabled, truncate(task, 80))

	_ = os.MkdirAll("/ipc/output", 0o755)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	obs := initObservability(ctx)
	defer func() {
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer shutdownCancel()
		if err := obs.shutdown(shutdownCtx); err != nil {
			log.Printf("failed to shutdown OTel providers: %v", err)
		}
	}()

	// Extract TRACEPARENT from env so the runner trace joins the controller trace.
	if tp := os.Getenv("TRACEPARENT"); tp != "" {
		log.Printf("TRACEPARENT env var found: %s", tp)
		prop := propagation.TraceContext{}
		carrier := propagation.MapCarrier{"traceparent": tp}
		ctx = prop.Extract(ctx, carrier)
		sc := oteltrace.SpanContextFromContext(ctx)
		log.Printf("after extraction: traceID=%s spanID=%s remote=%v valid=%v", sc.TraceID(), sc.SpanID(), sc.IsRemote(), sc.IsValid())
	} else {
		log.Println("TRACEPARENT env var not set")
	}

	ctx, runSpan := obs.startRunSpan(ctx,
		attribute.String("instance", getEnv("INSTANCE_NAME", "")),
		attribute.String("tenant.namespace", getEnv("AGENT_NAMESPACE", "")),
		attribute.String("model", modelName),
		attribute.String("task.summary", truncate(task, 200)),
	)
	writeTraceContextMetadata(ctx)
	logWithTrace(ctx, "info", "agent run started", map[string]any{
		"instance":  getEnv("INSTANCE_NAME", ""),
		"namespace": getEnv("AGENT_NAMESPACE", ""),
		"provider":  provider,
		"model":     modelName,
	})

	start := time.Now()

	var (
		responseText string
		inputTokens  int
		outputTokens int
		toolCalls    int
		err          error
	)

	switch provider {
	case "anthropic":
		responseText, inputTokens, outputTokens, toolCalls, err = callAnthropic(ctx, apiKey, baseURL, modelName, systemPrompt, task, tools)
	case "bedrock":
		responseText, inputTokens, outputTokens, toolCalls, err = callBedrock(ctx, modelName, systemPrompt, task, tools)
	default:
		// OpenAI, Azure OpenAI, Ollama, LM Studio, and any OpenAI-compatible provider
		responseText, inputTokens, outputTokens, toolCalls, err = callOpenAI(ctx, provider, apiKey, baseURL, modelName, systemPrompt, task, tools)
	}

	elapsed := time.Since(start)

	var res agentResult
	res.Metrics.DurationMs = elapsed.Milliseconds()
	res.Metrics.ToolCalls = toolCalls

	debugMode := getEnv("DEBUG", "") == "true"

	if err != nil {
		log.Printf("LLM call failed: %v", err)
		res.Status = "error"
		res.Error = err.Error()
		markSpanError(runSpan, err)
		runSpan.SetStatus(codes.Error, err.Error())
	} else {
		log.Printf("LLM call succeeded (tokens: in=%d out=%d, tool_calls=%d)", inputTokens, outputTokens, toolCalls)
		res.Status = "success"
		res.Response = responseText
		res.Metrics.InputTokens = inputTokens
		res.Metrics.OutputTokens = outputTokens
		runSpan.SetAttributes(
			attribute.Int("gen_ai.usage.input_tokens", inputTokens),
			attribute.Int("gen_ai.usage.output_tokens", outputTokens),
			attribute.Int("gen_ai.tool.call.count", toolCalls),
		)
		runSpan.SetStatus(codes.Ok, "")
	}

	// Extract and emit memory update before stripping markers from the response.
	if memoryEnabled && res.Response != "" {
		if memUpdate := extractMemoryUpdate(res.Response); memUpdate != "" {
			fmt.Fprintf(os.Stdout, "\n__SYMPOZIUM_MEMORY__%s__SYMPOZIUM_MEMORY_END__\n", memUpdate)
			log.Printf("emitted memory update (%d bytes)", len(memUpdate))
		}
	}

	// Strip internal markers from the response so they don't appear in the
	// TUI feed or channel messages. Keep them only if DEBUG is enabled.
	if !debugMode && res.Response != "" {
		res.Response = stripMemoryMarkers(res.Response)
		res.Response = stripActionMarkers(res.Response)
	}

	// Stream chunks are written incrementally during the LLM call by the
	// streamWriter, so no bulk stream-0.json is needed here.

	writeJSON("/ipc/output/result.json", res)

	// Signal sidecars (tool-executor, etc.) to exit by writing a done sentinel.
	_ = os.WriteFile("/ipc/done", []byte("done"), 0o644)

	// Print a structured marker to stdout so the controller can extract
	// the result from pod logs even after the IPC volume is gone.
	if markerBytes, err := json.Marshal(res); err == nil {
		fmt.Fprintf(os.Stdout, "\n__SYMPOZIUM_RESULT__%s__SYMPOZIUM_END__\n", string(markerBytes))
	}

	if res.Status == "error" {
		obs.recordRunMetrics(ctx, "error", getEnv("INSTANCE_NAME", ""), modelName, getEnv("AGENT_NAMESPACE", ""), elapsed.Milliseconds(), inputTokens, outputTokens)
		logWithTrace(ctx, "error", "agent run failed", map[string]any{"error": res.Error})
		runSpan.End()
		log.Printf("agent-runner finished with error: %s", res.Error)
		os.Exit(1)
	}
	obs.recordRunMetrics(ctx, "success", getEnv("INSTANCE_NAME", ""), modelName, getEnv("AGENT_NAMESPACE", ""), elapsed.Milliseconds(), inputTokens, outputTokens)
	logWithTrace(ctx, "info", "agent run succeeded", map[string]any{
		"duration_ms":   elapsed.Milliseconds(),
		"input_tokens":  inputTokens,
		"output_tokens": outputTokens,
		"tool_calls":    toolCalls,
	})
	runSpan.End()
	log.Println("agent-runner finished successfully")
}

// callAnthropic uses the official Anthropic Go SDK with streaming and optional
// tool calling. Text deltas are written as incremental stream-N.json files for
// real-time UI updates.
func callAnthropic(ctx context.Context, apiKey, baseURL, model, systemPrompt, task string, tools []ToolDef) (string, int, int, int, error) {
	opts := []anthropicoption.RequestOption{
		anthropicoption.WithMaxRetries(5),
	}
	if apiKey != "" {
		opts = append(opts, anthropicoption.WithAPIKey(apiKey))
	}
	if baseURL != "" {
		opts = append(opts, anthropicoption.WithBaseURL(baseURL))
	}

	client := anthropic.NewClient(opts...)

	// Build Anthropic tool definitions.
	var anthropicTools []anthropic.ToolUnionParam
	for _, t := range tools {
		schema := anthropic.ToolInputSchemaParam{
			Properties: t.Parameters["properties"],
		}
		if req, ok := t.Parameters["required"].([]string); ok {
			schema.Required = req
		}
		tool := anthropic.ToolUnionParamOfTool(schema, t.Name)
		tool.OfTool.Description = anthropic.String(t.Description)
		anthropicTools = append(anthropicTools, tool)
	}

	messages := []anthropic.MessageParam{
		anthropic.NewUserMessage(anthropic.NewTextBlock(task)),
	}

	totalInputTokens := 0
	totalOutputTokens := 0
	totalToolCalls := 0
	sw := &streamWriter{}
	statusIdx := 0

	for i := 0; i < maxToolIterations; i++ {
		statusID := fmt.Sprintf("status-%d", statusIdx)
		if i == 0 {
			sw.EmitToolCall(statusID, "Analyzing", "")
		}

		params := anthropic.MessageNewParams{
			Model:     anthropic.Model(model),
			MaxTokens: int64(8192),
			System: []anthropic.TextBlockParam{
				{Text: systemPrompt},
			},
			Messages: messages,
		}
		if len(anthropicTools) > 0 {
			params.Tools = anthropicTools
		}

		chatCtx, chatSpan := obs.startChatSpan(ctx,
			attribute.String("gen_ai.system", "anthropic"),
			attribute.String("gen_ai.request.model", model),
		)

		stream := client.Messages.NewStreaming(chatCtx, params)
		message := anthropic.Message{}

		for stream.Next() {
			event := stream.Current()
			_ = message.Accumulate(event)

			switch ev := event.AsAny().(type) {
			case anthropic.ContentBlockDeltaEvent:
				switch delta := ev.Delta.AsAny().(type) {
				case anthropic.TextDelta:
					sw.Write(delta.Text)
				}
			}
		}

		if err := stream.Err(); err != nil {
			markSpanError(chatSpan, err)
			chatSpan.End()
			var apiErr *anthropic.Error
			if errors.As(err, &apiErr) {
				return "", totalInputTokens, totalOutputTokens, totalToolCalls,
					fmt.Errorf("Anthropic API error (HTTP %d): %s", apiErr.StatusCode, truncate(apiErr.Error(), 500))
			}
			return "", totalInputTokens, totalOutputTokens, totalToolCalls,
				fmt.Errorf("Anthropic API error: %w", err)
		}

		totalInputTokens += int(message.Usage.InputTokens)
		totalOutputTokens += int(message.Usage.OutputTokens)
		chatSpan.SetAttributes(
			attribute.Int("gen_ai.usage.input_tokens", int(message.Usage.InputTokens)),
			attribute.Int("gen_ai.usage.output_tokens", int(message.Usage.OutputTokens)),
			attribute.String("gen_ai.response.finish_reasons", string(message.StopReason)),
		)
		chatSpan.SetStatus(codes.Ok, "")
		chatSpan.End()

		var textContent strings.Builder
		var toolUseBlocks []anthropic.ToolUseBlock
		for _, block := range message.Content {
			switch v := block.AsAny().(type) {
			case anthropic.TextBlock:
				textContent.WriteString(v.Text)
			case anthropic.ToolUseBlock:
				toolUseBlocks = append(toolUseBlocks, v)
			}
		}

		if message.StopReason != anthropic.StopReasonToolUse || len(toolUseBlocks) == 0 {
			sw.EmitToolResult(statusID, "success", "")
			sw.Flush()
			return textContent.String(), totalInputTokens, totalOutputTokens, totalToolCalls, nil
		}

		sw.EmitToolResult(statusID, "success", "")

		// Build the assistant message from accumulated content blocks.
		messages = append(messages, message.ToParam())

		var resultBlocks []anthropic.ContentBlockParamUnion
		for _, tu := range toolUseBlocks {
			totalToolCalls++
			log.Printf("tool_use [%d]: %s id=%s", totalToolCalls, tu.Name, tu.ID)

			sw.EmitToolCall(tu.ID, tu.Name, string(tu.Input))
			result := executeToolCallWithTelemetry(ctx, tu.Name, string(tu.Input), tu.ID)
			isErr := strings.HasPrefix(result, "Error:")
			status := "success"
			if isErr {
				status = "error"
			}
			sw.EmitToolResult(tu.ID, status, result)
			resultBlocks = append(resultBlocks, anthropic.NewToolResultBlock(tu.ID, result, isErr))
		}
		statusIdx++
		statusID = fmt.Sprintf("status-%d", statusIdx)
		sw.EmitToolCall(statusID, "Reasoning", "")
		messages = append(messages, anthropic.NewUserMessage(resultBlocks...))
	}

	return "", totalInputTokens, totalOutputTokens, totalToolCalls,
		fmt.Errorf("exceeded maximum tool-call iterations (%d)", maxToolIterations)
}

// callOpenAI uses the official OpenAI Go SDK with streaming and optional tool
// calling. Text deltas are written as incremental stream-N.json files that the
// IPC bridge picks up and publishes to NATS for real-time UI updates.
func callOpenAI(ctx context.Context, provider, apiKey, baseURL, model, systemPrompt, task string, tools []ToolDef) (string, int, int, int, error) {
	opts := []openaioption.RequestOption{
		openaioption.WithMaxRetries(5),
	}

	switch provider {
	case "azure-openai":
		if baseURL == "" {
			return "", 0, 0, 0, fmt.Errorf("Azure OpenAI requires MODEL_BASE_URL to be set")
		}
		apiVersion := getEnv("AZURE_OPENAI_API_VERSION", "2024-06-01")
		opts = append(opts,
			azure.WithEndpoint(baseURL, apiVersion),
			azure.WithAPIKey(apiKey),
		)
	default:
		if apiKey != "" {
			opts = append(opts, openaioption.WithAPIKey(apiKey))
		}
		if baseURL != "" {
			opts = append(opts, openaioption.WithBaseURL(baseURL))
		} else if provider == "ollama" {
			opts = append(opts, openaioption.WithBaseURL("http://ollama.default.svc:11434/v1"))
		} else if provider == "lm-studio" {
			opts = append(opts, openaioption.WithBaseURL("http://localhost:1234/v1"))
		}
	}

	client := openai.NewClient(opts...)

	// Build OpenAI tool definitions.
	var oaiTools []openai.ChatCompletionToolUnionParam
	for _, t := range tools {
		oaiTools = append(oaiTools, openai.ChatCompletionFunctionTool(shared.FunctionDefinitionParam{
			Name:        t.Name,
			Description: openai.String(t.Description),
			Parameters:  shared.FunctionParameters(t.Parameters),
		}))
	}

	messages := []openai.ChatCompletionMessageParamUnion{
		openai.SystemMessage(systemPrompt),
		openai.UserMessage(task),
	}

	totalInputTokens := 0
	totalOutputTokens := 0
	totalToolCalls := 0
	sw := &streamWriter{}
	statusIdx := 0

	for i := 0; i < maxToolIterations; i++ {
		statusID := fmt.Sprintf("status-%d", statusIdx)
		if i == 0 {
			sw.EmitToolCall(statusID, "Analyzing", "")
		}

		params := openai.ChatCompletionNewParams{
			Model:    openai.ChatModel(model),
			Messages: messages,
			StreamOptions: openai.ChatCompletionStreamOptionsParam{
				IncludeUsage: openai.Bool(true),
			},
		}
		if len(oaiTools) > 0 {
			params.Tools = oaiTools
		}

		chatCtx, chatSpan := obs.startChatSpan(ctx,
			attribute.String("gen_ai.system", provider),
			attribute.String("gen_ai.request.model", model),
		)

		stream := client.Chat.Completions.NewStreaming(chatCtx, params)
		acc := openai.ChatCompletionAccumulator{}

		for stream.Next() {
			chunk := stream.Current()
			acc.AddChunk(chunk)

			if len(chunk.Choices) > 0 && chunk.Choices[0].Delta.Content != "" {
				sw.Write(chunk.Choices[0].Delta.Content)
			}
		}

		if err := stream.Err(); err != nil {
			markSpanError(chatSpan, err)
			chatSpan.End()
			var apiErr *openai.Error
			if errors.As(err, &apiErr) {
				return "", totalInputTokens, totalOutputTokens, totalToolCalls,
					fmt.Errorf("OpenAI API error (HTTP %d): %s", apiErr.StatusCode, truncate(apiErr.Error(), 500))
			}
			return "", totalInputTokens, totalOutputTokens, totalToolCalls,
				fmt.Errorf("OpenAI API error: %w", err)
		}

		totalInputTokens += int(acc.Usage.PromptTokens)
		totalOutputTokens += int(acc.Usage.CompletionTokens)
		chatSpan.SetAttributes(
			attribute.Int("gen_ai.usage.input_tokens", int(acc.Usage.PromptTokens)),
			attribute.Int("gen_ai.usage.output_tokens", int(acc.Usage.CompletionTokens)),
		)

		if len(acc.Choices) == 0 {
			markSpanError(chatSpan, fmt.Errorf("no choices in completion response"))
			chatSpan.End()
			return "", totalInputTokens, totalOutputTokens, totalToolCalls,
				fmt.Errorf("no choices in completion response")
		}
		choice := acc.Choices[0]
		chatSpan.SetAttributes(attribute.String("gen_ai.response.finish_reasons", choice.FinishReason))
		chatSpan.SetStatus(codes.Ok, "")
		chatSpan.End()

		if choice.FinishReason == "tool_calls" && len(choice.Message.ToolCalls) > 0 {
			sw.EmitToolResult(statusID, "success", "")
			messages = append(messages, choice.Message.ToParam())

			for _, tc := range choice.Message.ToolCalls {
				totalToolCalls++
				log.Printf("tool_call [%d]: %s id=%s", totalToolCalls, tc.Function.Name, tc.ID)

				sw.EmitToolCall(tc.ID, tc.Function.Name, tc.Function.Arguments)
				result := executeToolCallWithTelemetry(ctx, tc.Function.Name, tc.Function.Arguments, tc.ID)
				status := "success"
				if strings.HasPrefix(result, "Error:") {
					status = "error"
				}
				sw.EmitToolResult(tc.ID, status, result)
				messages = append(messages, openai.ToolMessage(result, tc.ID))
			}
			statusIdx++
			statusID = fmt.Sprintf("status-%d", statusIdx)
			sw.EmitToolCall(statusID, "Reasoning", "")
			continue
		}

		sw.EmitToolResult(statusID, "success", "")
		sw.Flush()
		return choice.Message.Content, totalInputTokens, totalOutputTokens, totalToolCalls, nil
	}

	return "", totalInputTokens, totalOutputTokens, totalToolCalls,
		fmt.Errorf("exceeded maximum tool-call iterations (%d)", maxToolIterations)
}

func writeJSON(path string, v any) {
	dir := filepath.Dir(path)
	_ = os.MkdirAll(dir, 0o755)
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		log.Printf("WARNING: failed to marshal JSON for %s: %v", path, err)
		return
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		log.Printf("WARNING: failed to write %s: %v", path, err)
	}
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

func fatal(msg string) {
	log.Println("FATAL: " + msg)
	_ = os.MkdirAll("/ipc/output", 0o755)
	_ = os.WriteFile("/ipc/done", []byte("done"), 0o644)
	writeJSON("/ipc/output/result.json", agentResult{
		Status: "error",
		Error:  msg,
	})
	os.Exit(1)
}

// extractMemoryUpdate looks for a memory update block in the LLM response.
// The agent is instructed to wrap its memory updates in:
//
//	__SYMPOZIUM_MEMORY__
//	<content>
//	__SYMPOZIUM_MEMORY_END__
func extractMemoryUpdate(response string) string {
	const startMarker = "__SYMPOZIUM_MEMORY__"
	const endMarker = "__SYMPOZIUM_MEMORY_END__"

	startIdx := strings.LastIndex(response, startMarker)
	if startIdx < 0 {
		return ""
	}
	payload := response[startIdx+len(startMarker):]
	endIdx := strings.Index(payload, endMarker)
	if endIdx < 0 {
		return ""
	}
	return strings.TrimSpace(payload[:endIdx])
}

// stripMemoryMarkers removes all __SYMPOZIUM_MEMORY__...END__ blocks from the
// response text so they don't appear in the TUI feed or channel messages.
func stripMemoryMarkers(response string) string {
	return stripMarkerBlocks(response, "__SYMPOZIUM_MEMORY__", "__SYMPOZIUM_MEMORY_END__")
}

// stripActionMarkers removes all __SYMPOZIUM_ACTIONS__...END__ blocks.
func stripActionMarkers(response string) string {
	return stripMarkerBlocks(response, "__SYMPOZIUM_ACTIONS__", "__SYMPOZIUM_ACTIONS_END__")
}

func stripMarkerBlocks(response, startMarker, endMarker string) string {
	for {
		startIdx := strings.Index(response, startMarker)
		if startIdx < 0 {
			break
		}
		endIdx := strings.Index(response[startIdx:], endMarker)
		if endIdx < 0 {
			response = strings.TrimSpace(response[:startIdx])
			break
		}
		response = response[:startIdx] + response[startIdx+endIdx+len(endMarker):]
	}
	return strings.TrimSpace(response)
}
