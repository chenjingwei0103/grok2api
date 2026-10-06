package gateway

import (
	"bytes"
	"encoding/json"
	"sort"
	"strconv"
)

// schemaIndexArguments reports tool arguments that are only the JSON schema
// field order, such as {"kind":1,"mode":2,"name":3}. A real call has strings
// or other values. Four or more fields avoids ordinary numeric pairs.
func schemaIndexArguments(raw []byte) bool {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return false
	}
	if raw[0] == '"' {
		var decoded string
		if err := json.Unmarshal(raw, &decoded); err != nil {
			return false
		}
		raw = bytes.TrimSpace([]byte(decoded))
	}
	if len(raw) == 0 || raw[0] != '{' {
		return false
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || len(fields) < 4 {
		return false
	}
	seen := make([]int, 0, len(fields))
	for _, value := range fields {
		token := bytes.TrimSpace(value)
		if len(token) == 0 || token[0] < '0' || token[0] > '9' {
			return false
		}
		if bytes.ContainsAny(token, ".eE+") {
			return false
		}
		number, err := strconv.Atoi(string(token))
		if err != nil || strconv.Itoa(number) != string(token) || number < 1 {
			return false
		}
		seen = append(seen, number)
	}
	sort.Ints(seen)
	for i, number := range seen {
		if number != i+1 {
			return false
		}
	}
	return true
}

// stripSchemaIndexToolCalls removes copied schema-index function calls and
// their matching outputs from a Responses request. Other items stay untouched.
// The original body is returned when nothing matches.
func stripSchemaIndexToolCalls(body []byte) ([]byte, int) {
	if len(body) == 0 || (!bytes.Contains(body, []byte("function_call")) && !bytes.Contains(body, []byte("custom_tool_call"))) {
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
	type head struct {
		Type      string          `json:"type"`
		CallID    string          `json:"call_id"`
		Arguments json.RawMessage `json:"arguments"`
	}
	dropIDs := map[string]struct{}{}
	for _, item := range items {
		var parsed head
		if err := json.Unmarshal(item, &parsed); err != nil {
			continue
		}
		if parsed.Type != "function_call" && parsed.Type != "custom_tool_call" {
			continue
		}
		if !schemaIndexArguments(parsed.Arguments) || parsed.CallID == "" {
			continue
		}
		dropIDs[parsed.CallID] = struct{}{}
	}
	if len(dropIDs) == 0 {
		return body, 0
	}
	kept := make([]json.RawMessage, 0, len(items))
	removed := 0
	for _, item := range items {
		var parsed head
		if err := json.Unmarshal(item, &parsed); err != nil {
			kept = append(kept, item)
			continue
		}
		switch parsed.Type {
		case "function_call", "custom_tool_call":
			if schemaIndexArguments(parsed.Arguments) {
				removed++
				continue
			}
		case "function_call_output", "custom_tool_call_output":
			if _, drop := dropIDs[parsed.CallID]; drop {
				removed++
				continue
			}
		}
		kept = append(kept, item)
	}
	encodedInput, err := marshalRawJSON(kept)
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

func marshalRawJSON(value any) ([]byte, error) {
	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}
