package gateway

import (
	"bytes"
	"encoding/json"
	"strings"
)

// extractUserTextContent extracts concatenated plain text from user message item.
func extractUserTextContent(item map[string]json.RawMessage) string {
	rawContent := bytes.TrimSpace(item["content"])
	if len(rawContent) == 0 || bytes.Equal(rawContent, []byte("null")) {
		return ""
	}
	var text string
	if json.Unmarshal(rawContent, &text) == nil {
		return strings.TrimSpace(text)
	}
	var parts []map[string]json.RawMessage
	if json.Unmarshal(rawContent, &parts) == nil {
		var sb strings.Builder
		for _, p := range parts {
			t := strings.TrimSpace(rawString(p["type"]))
			if t == "" || t == "input_text" || t == "text" {
				sb.WriteString(rawString(p["text"]))
			}
		}
		return strings.TrimSpace(sb.String())
	}
	return ""
}

// stripTrailingSingleQuestionMarks removes redundant trailing user "?" or empty messages
// from the Responses input to prevent model repetition loops.
func stripTrailingSingleQuestionMarks(body []byte) ([]byte, int) {
	if len(body) == 0 || !bytes.Contains(body, []byte(`"role"`)) {
		return body, 0
	}
	var root map[string]json.RawMessage
	if err := json.Unmarshal(body, &root); err != nil {
		return body, 0
	}
	rawInput, ok := root["input"]
	if !ok {
		return body, 0
	}
	var items []json.RawMessage
	if err := json.Unmarshal(rawInput, &items); err != nil {
		return body, 0
	}
	if len(items) == 0 {
		return body, 0
	}
	// Check if trailing items are pure "?" questions
	removed := 0
	for len(items) > 1 {
		last := decodeRawObject(items[len(items)-1])
		if last == nil {
			break
		}
		role := strings.ToLower(strings.TrimSpace(rawString(last["role"])))
		if role != "user" {
			break
		}
		txt := strings.TrimSpace(extractUserTextContent(last))
		if txt == "?" || txt == "？" || txt == "" {
			items = items[:len(items)-1]
			removed++
		} else {
			break
		}
	}
	if removed == 0 {
		return body, 0
	}
	encodedInput, err := marshalRawJSON(items)
	if err != nil {
		return body, 0
	}
	root["input"] = encodedInput
	encoded, err := marshalRawJSON(root)
	if err != nil {
		return body, 0
	}
	return encoded, removed
}
