package gateway

import (
	"encoding/json"
	"testing"
)

func TestInjectActionDirectiveOnRetry(t *testing.T) {
	t.Parallel()

	// Case 1: normal conversation where previous turn was user message -> injects action directive
	bodyUser := []byte(`{"model":"grok-4.7","input":[{"type":"message","role":"user","content":"check status"}]}`)
	injectedUser := injectActionDirectiveOnRetry(bodyUser)
	var rootUser struct {
		Input []map[string]any `json:"input"`
	}
	if err := json.Unmarshal(injectedUser, &rootUser); err != nil {
		t.Fatalf("failed to unmarshal: %v", err)
	}
	if len(rootUser.Input) != 2 {
		t.Fatalf("expected 2 items, got %d", len(rootUser.Input))
	}
	lastContentUser := rootUser.Input[1]["content"].(string)
	if lastContentUser != retryActionDirectivePrompt {
		t.Fatalf("expected action directive %q, got %q", retryActionDirectivePrompt, lastContentUser)
	}

	// Case 2: idempotency / do not duplicate if already injected
	reInjectedUser := injectActionDirectiveOnRetry(injectedUser)
	if string(reInjectedUser) != string(injectedUser) {
		t.Fatalf("expected no-op on already injected body, got changed body")
	}

	// Case 3: trailing item is a function_call_output -> injects summary directive instead of command directive
	bodyToolOutput := []byte(`{"model":"grok-4.7","input":[{"type":"message","role":"user","content":"check status"},{"type":"function_call","call_id":"call_1","name":"bash","arguments":"{\"cmd\":\"df -h\"}"},{"type":"function_call_output","call_id":"call_1","output":"Filesystem Size Used Avail Use% Mounted on\n/dev/sda1 50G 20G 30G 40% /"}]}`)
	injectedTool := injectActionDirectiveOnRetry(bodyToolOutput)
	var rootTool struct {
		Input []map[string]any `json:"input"`
	}
	if err := json.Unmarshal(injectedTool, &rootTool); err != nil {
		t.Fatalf("failed to unmarshal: %v", err)
	}
	if len(rootTool.Input) != 4 {
		t.Fatalf("expected 4 items, got %d", len(rootTool.Input))
	}
	lastContentTool := rootTool.Input[3]["content"].(string)
	if lastContentTool != retrySummaryDirectivePrompt {
		t.Fatalf("expected summary directive %q, got %q", retrySummaryDirectivePrompt, lastContentTool)
	}

	// Case 4: idempotency for summary directive
	reInjectedTool := injectActionDirectiveOnRetry(injectedTool)
	if string(reInjectedTool) != string(injectedTool) {
		t.Fatalf("expected no-op on already injected summary directive, got changed body")
	}
}
