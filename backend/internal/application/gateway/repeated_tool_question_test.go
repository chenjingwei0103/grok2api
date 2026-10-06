package gateway

import (
	"bytes"
	"encoding/json"
	"testing"
)

func TestAppendQuestionForRepeatedToolCallResponses(t *testing.T) {
	t.Parallel()
	body := []byte(`{"model":"grok-4.7","input":[{"type":"message","role":"user","content":"check status"},{"type":"function_call","call_id":"call_1","name":"bash","arguments":"{\"cmd\":\"pwd\"}"},{"type":"function_call_output","call_id":"call_1","output":"E:/grok2api2"},{"type":"function_call","call_id":"call_2","name":"bash","arguments":"{\"cmd\":\"pwd\"}"},{"type":"function_call_output","call_id":"call_2","output":"E:/grok2api2"}]}`)

	got, repeated := appendQuestionForRepeatedToolCall(body)
	if !repeated {
		t.Fatal("expected repeated tool call to be detected")
	}
	var root struct {
		Input []map[string]any `json:"input"`
	}
	if err := json.Unmarshal(got, &root); err != nil {
		t.Fatal(err)
	}
	if len(root.Input) != 6 {
		t.Fatalf("input length = %d, want 6", len(root.Input))
	}
	last := root.Input[len(root.Input)-1]
	if last["type"] != "message" || last["role"] != "user" || last["content"] != "continue" {
		t.Fatalf("appended item = %#v", last)
	}
}

func TestAppendQuestionForRepeatedToolCallRequiresNoNewUserMessage(t *testing.T) {
	t.Parallel()
	body := []byte(`{"model":"grok-4.7","input":[{"type":"message","role":"user","content":"first"},{"type":"function_call","call_id":"call_1","name":"bash","arguments":"{\"cmd\":\"pwd\"}"},{"type":"function_call_output","call_id":"call_1","output":"one"},{"type":"message","role":"user","content":"new question"},{"type":"function_call","call_id":"call_2","name":"bash","arguments":"{\"cmd\":\"pwd\"}"},{"type":"function_call_output","call_id":"call_2","output":"one"}]}`)

	got, repeated := appendQuestionForRepeatedToolCall(body)
	if repeated {
		t.Fatal("a new user message must reset duplicate detection")
	}
	if string(got) != string(body) {
		t.Fatalf("body changed without duplicate: %s", got)
	}
}

func TestAppendQuestionForRepeatedToolCallIgnoresChangedOutput(t *testing.T) {
	t.Parallel()
	body := []byte(`{"model":"grok-4.7","input":[{"type":"message","role":"user","content":"check files"},{"type":"function_call","call_id":"call_1","name":"bash","arguments":"{\"cmd\":\"ls\"}"},{"type":"function_call_output","call_id":"call_1","output":"file-a"},{"type":"function_call","call_id":"call_2","name":"bash","arguments":"{\"cmd\":\"ls\"}"},{"type":"function_call_output","call_id":"call_2","output":"file-a\nfile-b"}]}`)

	got, repeated := appendQuestionForRepeatedToolCall(body)
	if repeated {
		t.Fatal("changed tool output must not be treated as an exact duplicate")
	}
	if string(got) != string(body) {
		t.Fatalf("body changed without duplicate: %s", got)
	}
}

func TestAppendQuestionForRepeatedToolCallCanonicalizesJSONArguments(t *testing.T) {
	t.Parallel()
	body := []byte(`{"model":"grok-4.7","input":[{"type":"message","role":"user","content":"check"},{"type":"function_call","call_id":"call_1","name":"bash","arguments":"{ \"path\": \"x\", \"cmd\": \"pwd\" }"},{"type":"function_call_output","call_id":"call_1","output":"same"},{"type":"function_call","call_id":"call_2","name":"bash","arguments":"{\"cmd\":\"pwd\",\"path\":\"x\"}"},{"type":"function_call_output","call_id":"call_2","output":"same"}]}`)

	got, repeated := appendQuestionForRepeatedToolCall(body)
	if !repeated {
		t.Fatal("JSON-equivalent arguments must be treated as the same tool call")
	}
	if !bytes.Contains(got, []byte(`"content":"continue"`)) {
		t.Fatalf("appended question missing from body: %s", got)
	}
}

func TestAppendQuestionForRepeatedToolCallDoesNotAppendTwice(t *testing.T) {
	t.Parallel()
	body := []byte(`{"model":"grok-4.7","input":[{"type":"message","role":"user","content":"check"},{"type":"function_call","call_id":"call_1","name":"bash","arguments":"{\"cmd\":\"pwd\"}"},{"type":"function_call_output","call_id":"call_1","output":"same"},{"type":"function_call","call_id":"call_2","name":"bash","arguments":"{\"cmd\":\"pwd\"}"},{"type":"function_call_output","call_id":"call_2","output":"same"},{"type":"message","role":"user","content":"continue"}]}`)

	got, repeated := appendQuestionForRepeatedToolCall(body)
	if repeated {
		t.Fatal("an already appended question must not trigger another append")
	}
	if string(got) != string(body) {
		t.Fatalf("body changed after existing question marker: %s", got)
	}
}

func TestAppendQuestionForRepeatedToolCallResetsAfterNewUserTurn(t *testing.T) {
	t.Parallel()
	body := []byte(`{"model":"grok-4.7","input":[{"type":"message","role":"user","content":"continue"},{"type":"message","role":"user","content":"new turn"},{"type":"function_call","call_id":"call_1","name":"bash","arguments":"{\"cmd\":\"pwd\"}"},{"type":"function_call_output","call_id":"call_1","output":"same"},{"type":"function_call","call_id":"call_2","name":"bash","arguments":"{\"cmd\":\"pwd\"}"},{"type":"function_call_output","call_id":"call_2","output":"same"}]}`)

	got, repeated := appendQuestionForRepeatedToolCall(body)
	if !repeated {
		t.Fatal("an earlier question marker must not suppress a later user turn")
	}
	var root struct {
		Input []map[string]any `json:"input"`
	}
	if err := json.Unmarshal(got, &root); err != nil {
		t.Fatal(err)
	}
	if len(root.Input) != 7 || root.Input[len(root.Input)-1]["content"] != "continue" {
		t.Fatalf("appended input = %#v", root.Input)
	}
}

func TestAppendQuestionForRepeatedCustomToolCall(t *testing.T) {
	t.Parallel()
	body := []byte(`{"model":"grok-4.7","input":[{"type":"message","role":"user","content":"apply the change"},{"type":"custom_tool_call","call_id":"call_1","name":"apply_patch","input":"patch"},{"type":"custom_tool_call_output","call_id":"call_1","output":"done"},{"type":"custom_tool_call","call_id":"call_2","name":"apply_patch","input":"patch"},{"type":"custom_tool_call_output","call_id":"call_2","output":"done"}]}`)

	got, repeated := appendQuestionForRepeatedToolCall(body)
	if !repeated {
		t.Fatal("expected repeated custom tool call to be detected")
	}
	var root struct {
		Input []map[string]any `json:"input"`
	}
	if err := json.Unmarshal(got, &root); err != nil {
		t.Fatal(err)
	}
	if len(root.Input) != 6 || root.Input[len(root.Input)-1]["content"] != "continue" {
		t.Fatalf("appended input = %#v", root.Input)
	}
}

func TestAppendQuestionForRepeatedToolCallTreatsImageOnlyUserMessageAsNewTurn(t *testing.T) {
	t.Parallel()
	body := []byte(`{"model":"grok-4.7","input":[{"type":"message","role":"user","content":"first"},{"type":"function_call","call_id":"call_1","name":"bash","arguments":"{\"cmd\":\"pwd\"}"},{"type":"function_call_output","call_id":"call_1","output":"same"},{"type":"message","role":"user","content":[{"type":"input_image","image_url":"https://example.test/image.png"}]},{"type":"function_call","call_id":"call_2","name":"bash","arguments":"{\"cmd\":\"pwd\"}"},{"type":"function_call_output","call_id":"call_2","output":"same"}]}`)

	got, repeated := appendQuestionForRepeatedToolCall(body)
	if repeated {
		t.Fatal("an image-only user message must reset duplicate detection")
	}
	if string(got) != string(body) {
		t.Fatalf("body changed without duplicate: %s", got)
	}
}
