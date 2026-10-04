package inference

import (
	"bytes"
	"testing"
)

func TestStripAutomationUpdateTools(t *testing.T) {
	t.Parallel()
	body := []byte(`{"model":"grok-4.6","prompt_cache_key":"sess-1","tools":[{"type":"function","name":"exec_command","parameters":{"type":"object"}},{"type":"namespace","name":"mcp__codex_app","tools":[{"type":"function","name":"automation_update","parameters":{"type":"object"}},{"type":"function","name":"list_threads","parameters":{"type":"object"}}]},{"type":"function","name":"codex_app__automation_update"}],"tool_choice":{"type":"function","name":"automation_update"},"input":[{"type":"function_call","name":"automation_update","arguments":"{\"kind\":1}"}]}`)
	got := stripAutomationUpdateTools(body)
	if bytes.Contains(got, []byte(`"name":"automation_update"`)) && bytes.Contains(got, []byte(`"tools"`)) {
		// Historical function_call stays. Declarations must not.
	}
	if bytes.Contains(got, []byte(`"name":"automation_update"`)) {
		if !bytes.Contains(got, []byte(`"type":"function_call"`)) {
			t.Fatalf("declaration survived: %s", got)
		}
	}
	for _, want := range []string{`"name":"exec_command"`, `"name":"list_threads"`, `"name":"mcp__codex_app"`, `"prompt_cache_key":"sess-1"`, `"tool_choice":"auto"`, `"type":"function_call"`} {
		if !bytes.Contains(got, []byte(want)) {
			t.Fatalf("missing %s in %s", want, got)
		}
	}
	if bytes.Contains(got, []byte("codex_app__automation_update")) {
		t.Fatalf("qualified tool survived: %s", got)
	}
}

func TestStripAutomationUpdateToolsChatAndEmptyNamespace(t *testing.T) {
	t.Parallel()
	body := []byte(`{"model":"grok-4.6","functions":[{"name":"automation_update"}],"tools":[{"type":"function","function":{"name":"automation_update","parameters":{"type":"object"}}},{"type":"namespace","name":"mcp__codex_app","tools":[{"name":"automation_update"}]}],"tool_choice":{"type":"function","function":{"name":"mcp__codex_app__automation_update"}}}`)
	got := stripAutomationUpdateTools(body)
	for _, gone := range []string{"automation_update", "mcp__codex_app"} {
		if bytes.Contains(got, []byte(gone)) {
			t.Fatalf("%s survived: %s", gone, got)
		}
	}
	if !bytes.Contains(got, []byte(`"functions":[]`)) || !bytes.Contains(got, []byte(`"tools":[]`)) || !bytes.Contains(got, []byte(`"tool_choice":"auto"`)) {
		t.Fatalf("body = %s", got)
	}
}

func TestStripAutomationUpdateToolsLeavesUnrelatedBody(t *testing.T) {
	t.Parallel()
	body := []byte("{\n  \"model\": \"grok-4.6\",\n  \"tools\": [{\"type\": \"function\", \"name\": \"exec_command\"}]\n}\n")
	got := stripAutomationUpdateTools(body)
	if !bytes.Equal(got, body) {
		t.Fatalf("unrelated body changed:\n%s", got)
	}
	mention := []byte(`{"model":"grok-4.6","input":"please do not call automation_update"}`)
	if !bytes.Equal(stripAutomationUpdateTools(mention), mention) {
		t.Fatal("prompt mention was rewritten")
	}
}
