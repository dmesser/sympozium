package main

import (
	"encoding/json"
	"fmt"
	"strings"
)

const (
	memStartMarker     = "__SYMPOZIUM_MEMORY__"
	memEndMarker       = "__SYMPOZIUM_MEMORY_END__"
	actionsStartMarker = "__SYMPOZIUM_ACTIONS__"
	actionsEndMarker   = "__SYMPOZIUM_ACTIONS_END__"
)

type suppressionKind int

const (
	suppressNone    suppressionKind = iota
	suppressMemory
	suppressActions
)

// streamWriter writes incremental stream-N.json chunk files to the IPC output
// directory. The IPC bridge watches for these files and publishes each as a
// TopicAgentStreamChunk NATS event. Memory markers are suppressed so they never
// reach the UI; actions markers are suppressed and emitted as structured
// suggestion chunks.
type streamWriter struct {
	chunkIdx     int
	pending      strings.Builder
	suppressKind suppressionKind
	suppressBuf  strings.Builder
}

// Write processes a text delta from the LLM stream. Text is emitted as
// stream-N.json files immediately unless it falls within a marker block:
//   - __SYMPOZIUM_MEMORY__...__SYMPOZIUM_MEMORY_END__: silently discarded
//   - __SYMPOZIUM_ACTIONS__...__SYMPOZIUM_ACTIONS_END__: parsed as JSON
//     and emitted as an "actions" stream chunk for UI suggestion buttons
func (sw *streamWriter) Write(delta string) {
	if sw.suppressKind != suppressNone {
		sw.suppressBuf.WriteString(delta)
		combined := sw.suppressBuf.String()

		var endMarker string
		if sw.suppressKind == suppressMemory {
			endMarker = memEndMarker
		} else {
			endMarker = actionsEndMarker
		}

		endIdx := strings.Index(combined, endMarker)
		if endIdx >= 0 {
			content := combined[:endIdx]
			remaining := combined[endIdx+len(endMarker):]
			wasActions := sw.suppressKind == suppressActions
			sw.suppressBuf.Reset()
			sw.suppressKind = suppressNone
			if wasActions {
				sw.emitActions(content)
			}
			if remaining != "" {
				sw.Write(remaining)
			}
		}
		return
	}

	sw.pending.WriteString(delta)
	text := sw.pending.String()

	memIdx := strings.Index(text, memStartMarker)
	actIdx := strings.Index(text, actionsStartMarker)

	markerStart := -1
	markerLen := 0
	kind := suppressNone

	if memIdx >= 0 && (actIdx < 0 || memIdx <= actIdx) {
		markerStart = memIdx
		markerLen = len(memStartMarker)
		kind = suppressMemory
	} else if actIdx >= 0 {
		markerStart = actIdx
		markerLen = len(actionsStartMarker)
		kind = suppressActions
	}

	if markerStart >= 0 {
		before := text[:markerStart]
		after := text[markerStart+markerLen:]
		sw.pending.Reset()
		if before != "" {
			sw.emitChunk(before)
		}
		sw.suppressKind = kind
		sw.suppressBuf.WriteString(after)

		var endMarker string
		if kind == suppressMemory {
			endMarker = memEndMarker
		} else {
			endMarker = actionsEndMarker
		}
		endIdx := strings.Index(sw.suppressBuf.String(), endMarker)
		if endIdx >= 0 {
			content := sw.suppressBuf.String()[:endIdx]
			remaining := sw.suppressBuf.String()[endIdx+len(endMarker):]
			wasActions := kind == suppressActions
			sw.suppressBuf.Reset()
			sw.suppressKind = suppressNone
			if wasActions {
				sw.emitActions(content)
			}
			if remaining != "" {
				sw.Write(remaining)
			}
		}
		return
	}

	// Check if text ends with a prefix of either start marker. If so, hold
	// those bytes until the next delta disambiguates them.
	safeLen := len(text)
	longest := len(actionsStartMarker) - 1
	if longest > len(text) {
		longest = len(text)
	}
	for prefixLen := longest; prefixLen >= 1; prefixLen-- {
		suffix := text[len(text)-prefixLen:]
		if strings.HasPrefix(memStartMarker, suffix) || strings.HasPrefix(actionsStartMarker, suffix) {
			safeLen = len(text) - prefixLen
			break
		}
	}

	if safeLen > 0 {
		safe := text[:safeLen]
		sw.pending.Reset()
		sw.pending.WriteString(text[safeLen:])
		sw.emitChunk(safe)
	}
}

// Flush emits any remaining buffered text that was being held for marker
// disambiguation. Call this after the final LLM iteration completes.
func (sw *streamWriter) Flush() {
	if sw.pending.Len() > 0 {
		sw.emitChunk(sw.pending.String())
		sw.pending.Reset()
	}
}

func (sw *streamWriter) emitChunk(content string) {
	if content == "" {
		return
	}
	writeJSON(fmt.Sprintf("/ipc/output/stream-%d.json", sw.chunkIdx), streamChunk{
		Type:    "text",
		Content: content,
		Index:   sw.chunkIdx,
	})
	sw.chunkIdx++
}

func (sw *streamWriter) emitActions(raw string) {
	raw = strings.TrimSpace(raw)
	var suggestions []actionSuggestion
	if err := json.Unmarshal([]byte(raw), &suggestions); err != nil {
		// Fallback: try parsing as plain string array for backwards compat
		var plain []string
		if err2 := json.Unmarshal([]byte(raw), &plain); err2 != nil {
			return
		}
		for _, s := range plain {
			suggestions = append(suggestions, actionSuggestion{Label: s, Prompt: s})
		}
	}
	if len(suggestions) == 0 {
		return
	}
	writeJSON(fmt.Sprintf("/ipc/output/stream-%d.json", sw.chunkIdx), streamChunk{
		Type:        "actions",
		Index:       sw.chunkIdx,
		Suggestions: suggestions,
	})
	sw.chunkIdx++
}

// EmitToolCall writes a tool_call stream chunk so the UI can show a badge
// for the tool being executed.
func (sw *streamWriter) EmitToolCall(id, name, args string) {
	writeJSON(fmt.Sprintf("/ipc/output/stream-%d.json", sw.chunkIdx), streamChunk{
		Type:     "tool_call",
		Index:    sw.chunkIdx,
		ToolID:   id,
		ToolName: name,
		ToolArgs: truncate(args, 500),
	})
	sw.chunkIdx++
}

// EmitToolResult writes a tool_result stream chunk so the UI can update the
// badge with success/error status.
func (sw *streamWriter) EmitToolResult(id, status, content string) {
	writeJSON(fmt.Sprintf("/ipc/output/stream-%d.json", sw.chunkIdx), streamChunk{
		Type:    "tool_result",
		Index:   sw.chunkIdx,
		ToolID:  id,
		Status:  status,
		Content: truncate(content, 10000),
	})
	sw.chunkIdx++
}
