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

	// Case 5: Chat API messages with trailing tool message -> injects summary directive
	bodyChatTool := []byte(`{"model":"grok-4.7","messages":[{"role":"user","content":"check disk"},{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"bash","arguments":"{\"cmd\":\"df -h\"}"}}]},{"role":"tool","tool_call_id":"call_1","content":"Filesystem 50G"}]}`)
	injectedChatTool := injectActionDirectiveOnRetry(bodyChatTool)
	var rootChatTool struct {
		Messages []map[string]any `json:"messages"`
	}
	if err := json.Unmarshal(injectedChatTool, &rootChatTool); err != nil {
		t.Fatalf("failed to unmarshal chat: %v", err)
	}
	if len(rootChatTool.Messages) != 4 {
		t.Fatalf("expected 4 messages, got %d", len(rootChatTool.Messages))
	}
	lastContentChat := rootChatTool.Messages[3]["content"].(string)
	if lastContentChat != retrySummaryDirectivePrompt {
		t.Fatalf("expected chat summary directive %q, got %q", retrySummaryDirectivePrompt, lastContentChat)
	}

	// Case 6: Chat API messages with trailing user message -> injects action directive
	bodyChatUser := []byte(`{"model":"grok-4.7","messages":[{"role":"user","content":"please fix it"}]}`)
	injectedChatUser := injectActionDirectiveOnRetry(bodyChatUser)
	var rootChatUser struct {
		Messages []map[string]any `json:"messages"`
	}
	if err := json.Unmarshal(injectedChatUser, &rootChatUser); err != nil {
		t.Fatalf("failed to unmarshal chat user: %v", err)
	}
	if len(rootChatUser.Messages) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(rootChatUser.Messages))
	}
	lastContentChatUser := rootChatUser.Messages[1]["content"].(string)
	if lastContentChatUser != retryActionDirectivePrompt {
		t.Fatalf("expected chat action directive %q, got %q", retryActionDirectivePrompt, lastContentChatUser)
	}
}
