package inference

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
)

const automationUpdateToolName = "automation_update"

// stripAutomationUpdateTools removes Codex automation_update declarations from
// a Responses or Chat Completions body before it is accepted. Nested namespace
// tools are removed too, and an empty namespace is dropped. A tool_choice that
// names the blocked tool becomes "auto". Session identity is left untouched.
// The original body is returned when nothing matches.
func stripAutomationUpdateTools(body []byte) []byte {
	if len(body) == 0 || !bytes.Contains(body, []byte(automationUpdateToolName)) {
		return body
	}
	updated, changed, err := stripAutomationObject(body)
	if err != nil || !changed {
		return body
	}
	return updated
}

type rawField struct {
	Key string
	Val json.RawMessage
}

func stripAutomationObject(raw []byte) ([]byte, bool, error) {
	fields, err := decodeObject(raw)
	if err != nil {
		return raw, false, err
	}
	changed := false
	for i, field := range fields {
		switch field.Key {
		case "tools", "functions":
			next, removed, err := stripToolArray(field.Val)
			if err != nil {
				return raw, false, err
			}
			if removed > 0 {
				fields[i].Val = next
				changed = true
			}
		case "tool_choice":
			next, ok := rewriteAutomationToolChoice(field.Val)
			if ok {
				fields[i].Val = next
				changed = true
			}
		}
	}
	if !changed {
		return raw, false, nil
	}
	return encodeObject(fields), true, nil
}

func stripToolArray(raw json.RawMessage) (json.RawMessage, int, error) {
	trim := bytes.TrimSpace(raw)
	if len(trim) == 0 || trim[0] != '[' {
		return raw, 0, nil
	}
	dec := json.NewDecoder(bytes.NewReader(trim))
	tok, err := dec.Token()
	if err != nil || tok != json.Delim('[') {
		return raw, 0, err
	}
	kept := make([]json.RawMessage, 0)
	removed := 0
	for dec.More() {
		var item json.RawMessage
		if err := dec.Decode(&item); err != nil {
			return raw, 0, err
		}
		if isBlockedTool(item) {
			removed++
			continue
		}
		next, nested, err := stripNestedTools(item)
		if err != nil {
			return raw, 0, err
		}
		removed += nested
		if nested > 0 && isEmptyNamespace(next) {
			continue
		}
		kept = append(kept, next)
	}
	if _, err := dec.Token(); err != nil {
		return raw, 0, err
	}
	if removed == 0 {
		return raw, 0, nil
	}
	return encodeArray(kept), removed, nil
}

func stripNestedTools(item json.RawMessage) (json.RawMessage, int, error) {
	fields, err := decodeObject(item)
	if err != nil {
		return item, 0, nil
	}
	removed := 0
	for i, field := range fields {
		if field.Key != "tools" {
			continue
		}
		next, n, err := stripToolArray(field.Val)
		if err != nil {
			return item, 0, err
		}
		if n > 0 {
			fields[i].Val = next
			removed += n
		}
	}
	if removed == 0 {
		return item, 0, nil
	}
	return encodeObject(fields), removed, nil
}

func isBlockedTool(item json.RawMessage) bool {
	for _, name := range toolNames(item) {
		if strings.Contains(strings.ToLower(name), automationUpdateToolName) {
			return true
		}
	}
	return false
}

func toolNames(item json.RawMessage) []string {
	fields, err := decodeObject(item)
	if err != nil {
		return nil
	}
	names := make([]string, 0, 2)
	for _, field := range fields {
		switch field.Key {
		case "name", "tool":
			if value, ok := jsonString(field.Val); ok {
				names = append(names, value)
			}
		case "function":
			for _, nested := range mustFields(field.Val) {
				if nested.Key == "name" {
					if value, ok := jsonString(nested.Val); ok {
						names = append(names, value)
					}
				}
			}
		}
	}
	return names
}

func mustFields(raw json.RawMessage) []rawField {
	fields, err := decodeObject(raw)
	if err != nil {
		return nil
	}
	return fields
}

func isEmptyNamespace(item json.RawMessage) bool {
	fields, err := decodeObject(item)
	if err != nil {
		return false
	}
	typeName := ""
	toolsEmpty := false
	hasTools := false
	for _, field := range fields {
		switch field.Key {
		case "type":
			value, _ := jsonString(field.Val)
			typeName = strings.ToLower(value)
		case "tools":
			hasTools = true
			toolsEmpty = bytes.Equal(bytes.TrimSpace(field.Val), []byte("[]"))
		}
	}
	return typeName == "namespace" && hasTools && toolsEmpty
}

func rewriteAutomationToolChoice(raw json.RawMessage) (json.RawMessage, bool) {
	trim := bytes.TrimSpace(raw)
	if len(trim) == 0 {
		return raw, false
	}
	if trim[0] == '"' {
		value, ok := jsonString(trim)
		if ok && strings.Contains(strings.ToLower(value), automationUpdateToolName) {
			return json.RawMessage(`"auto"`), true
		}
		return raw, false
	}
	if trim[0] == '{' && bytes.Contains(bytes.ToLower(trim), []byte(automationUpdateToolName)) {
		return json.RawMessage(`"auto"`), true
	}
	return raw, false
}

func jsonString(raw json.RawMessage) (string, bool) {
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", false
	}
	return value, true
}

func decodeObject(raw []byte) ([]rawField, error) {
	dec := json.NewDecoder(bytes.NewReader(bytes.TrimSpace(raw)))
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	if tok != json.Delim('{') {
		return nil, errNotObject
	}
	fields := make([]rawField, 0)
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		key, ok := keyTok.(string)
		if !ok {
			return nil, errNotObject
		}
		var val json.RawMessage
		if err := dec.Decode(&val); err != nil {
			return nil, err
		}
		fields = append(fields, rawField{Key: key, Val: val})
	}
	if _, err := dec.Token(); err != nil {
		return nil, err
	}
	return fields, nil
}

var errNotObject = errors.New("json value is not an object")

func encodeObject(fields []rawField) []byte {
	var buf bytes.Buffer
	buf.WriteByte('{')
	for i, field := range fields {
		if i > 0 {
			buf.WriteByte(',')
		}
		key, _ := json.Marshal(field.Key)
		buf.Write(key)
		buf.WriteByte(':')
		buf.Write(bytes.TrimSpace(field.Val))
	}
	buf.WriteByte('}')
	return buf.Bytes()
}

func encodeArray(items []json.RawMessage) json.RawMessage {
	var buf bytes.Buffer
	buf.WriteByte('[')
	for i, item := range items {
		if i > 0 {
			buf.WriteByte(',')
		}
		buf.Write(bytes.TrimSpace(item))
	}
	buf.WriteByte(']')
	return buf.Bytes()
}
