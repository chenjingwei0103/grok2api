package gateway

import (
	"bytes"
	"encoding/json"
	"strings"
)

// appendQuestionForRepeatedToolCall adds one explicit user question to a
// Responses request when the same completed tool call appears twice without a
// new user message. The question is intentionally added to the upstream body
// only; the original client request remains unchanged for audit/replay.
func appendQuestionForRepeatedToolCall(body []byte) ([]byte, bool) {
	var root map[string]json.RawMessage
	if len(body) == 0 || json.Unmarshal(body, &root) != nil {
		return body, false
	}
	rawInput, ok := root["input"]
	if !ok {
		return body, false
	}
	var items []json.RawMessage
	if json.Unmarshal(rawInput, &items) != nil {
		return body, false
	}
	if !hasRepeatedCompletedToolCall(items) {
		return body, false
	}
	if hasQuestionUserMessage(items) {
		return body, false
	}
	items = append(items, json.RawMessage(`{"type":"message","role":"user","content":"?"}`))
	encodedInput, err := json.Marshal(items)
	if err != nil {
		return body, false
	}
	root["input"] = encodedInput
	encoded, err := json.Marshal(root)
	if err != nil {
		return body, false
	}
	return encoded, true
}

type repeatedToolCallRecord struct {
	name      string
	arguments string
}

func hasRepeatedCompletedToolCall(items []json.RawMessage) bool {
	pending := make(map[string]repeatedToolCallRecord)
	completed := make(map[string]struct{})
	for _, raw := range items {
		item := decodeRawObject(raw)
		if item == nil {
			continue
		}
		if isMeaningfulUserInput(item) {
			pending = make(map[string]repeatedToolCallRecord)
			completed = make(map[string]struct{})
			continue
		}
		typeName := strings.TrimSpace(rawString(item["type"]))
		switch typeName {
		case "function_call", "custom_tool_call":
			callID := strings.TrimSpace(rawString(item["call_id"]))
			name := strings.TrimSpace(rawString(item["name"]))
			if callID == "" || name == "" {
				continue
			}
			pending[callID] = repeatedToolCallRecord{
				name:      name,
				arguments: canonicalToolJSON(toolCallInput(item, typeName)),
			}
		case "function_call_output", "custom_tool_call_output":
			callID := strings.TrimSpace(rawString(item["call_id"]))
			call, ok := pending[callID]
			if !ok || call.arguments == "" {
				continue
			}
			key := call.name + "\x00" + call.arguments + "\x00" + canonicalToolJSON(item["output"])
			if _, exists := completed[key]; exists {
				return true
			}
			completed[key] = struct{}{}
		}
	}
	return false
}

func hasQuestionUserMessage(items []json.RawMessage) bool {
	markerInCurrentTurn := false
	for _, raw := range items {
		item := decodeRawObject(raw)
		if item == nil || !isMeaningfulUserInput(item) {
			continue
		}
		if strings.TrimSpace(userInputText(item)) == "?" {
			markerInCurrentTurn = true
			continue
		}
		markerInCurrentTurn = false
	}
	return markerInCurrentTurn
}

func isMeaningfulUserInput(item map[string]json.RawMessage) bool {
	role := strings.ToLower(strings.TrimSpace(rawString(item["role"])))
	if role != "user" {
		return false
	}
	rawContent := bytes.TrimSpace(item["content"])
	if len(rawContent) == 0 || bytes.Equal(rawContent, []byte("null")) {
		return false
	}
	var text string
	if json.Unmarshal(rawContent, &text) == nil {
		return strings.TrimSpace(text) != ""
	}
	var parts []json.RawMessage
	if json.Unmarshal(rawContent, &parts) == nil {
		return len(parts) > 0
	}
	return true
}

func toolCallInput(item map[string]json.RawMessage, typeName string) json.RawMessage {
	if typeName == "custom_tool_call" {
		return item["input"]
	}
	return item["arguments"]
}

func userInputText(item map[string]json.RawMessage) string {
	raw := bytes.TrimSpace(item["content"])
	if len(raw) == 0 {
		return ""
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text
	}
	var parts []map[string]json.RawMessage
	if json.Unmarshal(raw, &parts) != nil {
		return ""
	}
	var builder strings.Builder
	for _, part := range parts {
		partType := strings.TrimSpace(rawString(part["type"]))
		switch partType {
		case "input_text", "text", "output_text":
			builder.WriteString(rawString(part["text"]))
		case "refusal":
			builder.WriteString(rawString(part["refusal"]))
		}
	}
	return builder.String()
}

func decodeRawObject(raw json.RawMessage) map[string]json.RawMessage {
	var item map[string]json.RawMessage
	if json.Unmarshal(raw, &item) != nil {
		return nil
	}
	return item
}

func rawString(raw json.RawMessage) string {
	var value string
	if len(bytes.TrimSpace(raw)) == 0 || json.Unmarshal(raw, &value) != nil {
		return ""
	}
	return value
}

func canonicalToolJSON(raw json.RawMessage) string {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return ""
	}
	var text string
	if json.Unmarshal(trimmed, &text) == nil {
		text = strings.TrimSpace(text)
		var nested any
		if json.Unmarshal([]byte(text), &nested) == nil {
			if encoded, err := json.Marshal(nested); err == nil {
				return string(encoded)
			}
		}
		return text
	}
	var value any
	if json.Unmarshal(trimmed, &value) == nil {
		encoded, err := json.Marshal(value)
		if err == nil {
			return string(encoded)
		}
	}
	return string(trimmed)
}
