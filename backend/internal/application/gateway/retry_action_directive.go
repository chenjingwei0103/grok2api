package gateway

import (
	"encoding/json"
	"strings"
)

const retryActionDirectivePrompt = "请实际调用命令执行并验证，禁止直接凭记忆回复完成。"

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
	// Do not append if already present
	if len(items) > 0 {
		last := decodeRawObject(items[len(items)-1])
		if last != nil && strings.TrimSpace(extractUserTextContent(last)) == retryActionDirectivePrompt {
			return body
		}
	}
	promptJSON, _ := json.Marshal(retryActionDirectivePrompt)
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
