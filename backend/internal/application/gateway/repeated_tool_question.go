package gateway

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
)

const repeatedToolContinueThreshold = 3

// toolLoopBreakoutInstruction 针对检测到的重复调用死循环注入破局引导提示
const toolLoopBreakoutInstruction = "Notice: The previous tool call has failed repeatedly with the same output. Please stop repeating this identical command. Inspect the error output carefully, change your syntax, parameters, or approach, or reply directly with an explanation."

// ToolLoopEvidence contains redacted evidence for a repeated tool-call tail.
// It deliberately stores hashes instead of arguments or tool output.
type ToolLoopEvidence struct {
	Detected         bool
	ToolName         string
	ArgumentsHash    string
	OutputHash       string
	ConsecutiveCount int
}

// appendContinueForTrailingToolLoop adds one upstream-only continue marker
// after a genuinely repeated tail of completed tool calls. The client body
// remains unchanged so the marker cannot become part of the next turn.
func appendContinueForTrailingToolLoop(body []byte) ([]byte, ToolLoopEvidence) {
	var root map[string]json.RawMessage
	var evidence ToolLoopEvidence
	if len(body) == 0 || json.Unmarshal(body, &root) != nil {
		return body, evidence
	}
	rawInput, ok := root["input"]
	if !ok {
		return body, evidence
	}
	var items []json.RawMessage
	if json.Unmarshal(rawInput, &items) != nil {
		return body, evidence
	}
	evidence = detectTrailingToolLoop(items)
	if !evidence.Detected {
		return body, evidence
	}
	promptJSON, _ := json.Marshal(toolLoopBreakoutInstruction)
	items = append(items, json.RawMessage(`{"type":"message","role":"user","content":`+string(promptJSON)+`}`))
	encodedInput, err := json.Marshal(items)
	if err != nil {
		return body, ToolLoopEvidence{}
	}
	root["input"] = encodedInput
	encoded, err := json.Marshal(root)
	if err != nil {
		return body, ToolLoopEvidence{}
	}
	return encoded, evidence
}

func detectTrailingToolLoop(items []json.RawMessage) ToolLoopEvidence {
	var evidence ToolLoopEvidence
	pending := make(map[string]repeatedToolCallRecord)
	lastFingerprint := ""
	consecutive := 0
	for _, raw := range items {
		item := decodeRawObject(raw)
		if item == nil {
			continue
		}
		if isMeaningfulUserInput(item) {
			pending = make(map[string]repeatedToolCallRecord)
			lastFingerprint = ""
			consecutive = 0
			evidence = ToolLoopEvidence{}
			continue
		}
		typeName := strings.TrimSpace(rawString(item["type"]))
		switch typeName {
		case "function_call", "custom_tool_call":
			callID := strings.TrimSpace(rawString(item["call_id"]))
			name := strings.TrimSpace(rawString(item["name"]))
			arguments := canonicalToolJSON(toolCallInput(item, typeName))
			if callID == "" || name == "" || arguments == "" {
				pending = make(map[string]repeatedToolCallRecord)
				lastFingerprint = ""
				consecutive = 0
				evidence = ToolLoopEvidence{}
				continue
			}
			pending[callID] = repeatedToolCallRecord{name: name, arguments: arguments}
		case "function_call_output", "custom_tool_call_output":
			callID := strings.TrimSpace(rawString(item["call_id"]))
			call, ok := pending[callID]
			if !ok {
				pending = make(map[string]repeatedToolCallRecord)
				lastFingerprint = ""
				consecutive = 0
				evidence = ToolLoopEvidence{}
				continue
			}
			delete(pending, callID)
			output := canonicalToolJSON(item["output"])
			fingerprint := call.name + "\x00" + call.arguments + "\x00" + output
			if fingerprint == lastFingerprint {
				consecutive++
			} else {
				consecutive = 1
				evidence = ToolLoopEvidence{}
			}
			lastFingerprint = fingerprint
			if consecutive >= repeatedToolContinueThreshold {
				evidence = ToolLoopEvidence{
					Detected:         true,
					ToolName:         call.name,
					ArgumentsHash:    shortToolLoopHash(call.arguments),
					OutputHash:       shortToolLoopHash(output),
					ConsecutiveCount: consecutive,
				}
			}
		case "reasoning":
			// Reasoning items can be interleaved with a tool call/output pair
			// in Responses history; they do not make the tool tail non-contiguous.
			continue
		default:
			pending = make(map[string]repeatedToolCallRecord)
			lastFingerprint = ""
			consecutive = 0
			evidence = ToolLoopEvidence{}
		}
	}
	if len(pending) > 0 {
		return ToolLoopEvidence{}
	}
	return evidence
}

func shortToolLoopHash(value string) string {
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:])[:16]
}

type repeatedToolCallRecord struct {
	name      string
	arguments string
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
