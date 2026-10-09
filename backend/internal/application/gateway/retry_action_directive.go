package gateway

import (
	"encoding/json"
	"strings"
)

const retryActionDirectivePrompt = "请实际调用命令执行并验证，禁止直接凭记忆回复完成。"
const retrySummaryDirectivePrompt = "请结合刚才的实际命令输出认真思考分析并给出结论，不要复读上一轮的开场白。"

// injectActionDirectiveOnRetry appends an explicit action directive user message
// to upstreamBody on subsequent quality retry attempts if the previous attempt
// was withheld due to missing reasoning.
func injectActionDirectiveOnRetry(body []byte) []byte {
	if len(body) == 0 {
		return body
	}
	var root map[string]json.RawMessage
	if err := json.Unmarshal(body, &root); err != nil {
		return body
	}
	rawInput, ok := root["input"]
	if !ok {
		return body
	}
	var items []json.RawMessage
	if err := json.Unmarshal(rawInput, &items); err != nil {
		return body
	}
	directivePrompt := retryActionDirectivePrompt
	if hasTrailingToolOutput(items) {
		directivePrompt = retrySummaryDirectivePrompt
	}

	// Do not append if already present
	if len(items) > 0 {
		last := decodeRawObject(items[len(items)-1])
		if last != nil {
			txt := strings.TrimSpace(extractUserTextContent(last))
			if txt == retryActionDirectivePrompt || txt == retrySummaryDirectivePrompt {
				return body
			}
		}
	}
	promptJSON, _ := json.Marshal(directivePrompt)
	items = append(items, json.RawMessage(`{"type":"message","role":"user","content":`+string(promptJSON)+`}`))
	encodedInput, err := marshalRawJSON(items)
	if err != nil {
		return body
	}
	root["input"] = encodedInput
	encoded, err := marshalRawJSON(root)
	if err != nil {
		return body
	}
	return encoded
}

// hasTrailingToolOutput reports whether the last meaningful item in the input
// is a tool execution result (function_call_output, custom_tool_call_output, or role: tool).
func hasTrailingToolOutput(items []json.RawMessage) bool {
	for i := len(items) - 1; i >= 0; i-- {
		item := decodeRawObject(items[i])
		if item == nil {
			continue
		}
		typeName := strings.TrimSpace(rawString(item["type"]))
		switch typeName {
		case "function_call_output", "custom_tool_call_output":
			return true
		case "message":
			role := strings.ToLower(strings.TrimSpace(rawString(item["role"])))
			if role == "tool" {
				return true
			}
		}
		if typeName == "function_call" || typeName == "custom_tool_call" || typeName == "message" {
			break
		}
	}
	return false
}
