package gateway

import (
	"encoding/json"
	"testing"
)

func TestAppendContinueForTrailingToolLoopAppendsAfterThreeEquivalentCalls(t *testing.T) {
	t.Parallel()
	body := []byte(`{"model":"grok-4.7","input":[{"type":"message","role":"user","content":"inspect the repository"},{"type":"function_call","call_id":"call_1","name":"run_terminal_command","arguments":"{ \"path\": \"E:/grok2api2\", \"cmd\": \"git status --short\" }"},{"type":"function_call_output","call_id":"call_1","output":" M service.go"},{"type":"function_call","call_id":"call_2","name":"run_terminal_command","arguments":"{\"cmd\":\"git status --short\",\"path\":\"E:/grok2api2\"}"},{"type":"function_call_output","call_id":"call_2","output":" M service.go"},{"type":"function_call","call_id":"call_3","name":"run_terminal_command","arguments":"{\"path\":\"E:/grok2api2\",\"cmd\":\"git status --short\"}"},{"type":"function_call_output","call_id":"call_3","output":" M service.go"}]}`)

	got, evidence := appendContinueForTrailingToolLoop(body)
	if !evidence.Detected {
		t.Fatal("three identical trailing completed calls must be detected")
	}
	if evidence.ConsecutiveCount != 3 {
		t.Fatalf("consecutive count = %d, want 3", evidence.ConsecutiveCount)
	}
	if evidence.ToolName != "run_terminal_command" || evidence.ArgumentsHash == "" || evidence.OutputHash == "" {
		t.Fatalf("evidence = %#v", evidence)
	}
	assertTrailingContinue(t, got)
}

func TestAppendContinueForTrailingToolLoopDoesNotAppendForTwoCalls(t *testing.T) {
	t.Parallel()
	body := []byte(`{"model":"grok-4.7","input":[{"type":"message","role":"user","content":"inspect"},{"type":"function_call","call_id":"call_1","name":"bash","arguments":"{\"cmd\":\"pwd\"}"},{"type":"function_call_output","call_id":"call_1","output":"E:/grok2api2"},{"type":"function_call","call_id":"call_2","name":"bash","arguments":"{\"cmd\":\"pwd\"}"},{"type":"function_call_output","call_id":"call_2","output":"E:/grok2api2"}]}`)

	got, evidence := appendContinueForTrailingToolLoop(body)
	if evidence.Detected {
		t.Fatalf("two repeated calls must not be detected: %#v", evidence)
	}
	if string(got) != string(body) {
		t.Fatalf("body changed without a loop: %s", got)
	}
}

func TestAppendContinueForTrailingToolLoopRequiresSameOutput(t *testing.T) {
	t.Parallel()
	body := []byte(`{"model":"grok-4.7","input":[{"type":"message","role":"user","content":"inspect"},{"type":"function_call","call_id":"call_1","name":"bash","arguments":"{\"cmd\":\"ls\"}"},{"type":"function_call_output","call_id":"call_1","output":"file-a"},{"type":"function_call","call_id":"call_2","name":"bash","arguments":"{\"cmd\":\"ls\"}"},{"type":"function_call_output","call_id":"call_2","output":"file-a\nfile-b"},{"type":"function_call","call_id":"call_3","name":"bash","arguments":"{\"cmd\":\"ls\"}"},{"type":"function_call_output","call_id":"call_3","output":"file-a\nfile-b\nfile-c"}]}`)

	got, evidence := appendContinueForTrailingToolLoop(body)
	if evidence.Detected {
		t.Fatalf("changed output must break the loop: %#v", evidence)
	}
	if string(got) != string(body) {
		t.Fatalf("body changed without a loop: %s", got)
	}
}

func TestAppendContinueForTrailingToolLoopResetsAfterImageUserTurn(t *testing.T) {
	t.Parallel()
	body := []byte(`{"model":"grok-4.7","input":[{"type":"message","role":"user","content":"first"},{"type":"function_call","call_id":"call_1","name":"bash","arguments":"{\"cmd\":\"pwd\"}"},{"type":"function_call_output","call_id":"call_1","output":"E:/grok2api2"},{"type":"function_call","call_id":"call_2","name":"bash","arguments":"{\"cmd\":\"pwd\"}"},{"type":"function_call_output","call_id":"call_2","output":"E:/grok2api2"},{"type":"function_call","call_id":"call_3","name":"bash","arguments":"{\"cmd\":\"pwd\"}"},{"type":"function_call_output","call_id":"call_3","output":"E:/grok2api2"},{"type":"message","role":"user","content":[{"type":"input_image","image_url":"https://example.test/image.png"}]},{"type":"function_call","call_id":"call_4","name":"bash","arguments":"{\"cmd\":\"pwd\"}"},{"type":"function_call_output","call_id":"call_4","output":"E:/grok2api2"}]}`)

	got, evidence := appendContinueForTrailingToolLoop(body)
	if evidence.Detected {
		t.Fatalf("a new image user turn must reset the loop: %#v", evidence)
	}
	if string(got) != string(body) {
		t.Fatalf("body changed after a new user turn: %s", got)
	}
}

func TestAppendContinueForTrailingToolLoopIgnoresReasoningItems(t *testing.T) {
	t.Parallel()
	body := []byte(`{"model":"grok-4.7","input":[{"type":"message","role":"user","content":"inspect"},{"type":"reasoning","summary":[]},{"type":"function_call","call_id":"call_1","name":"bash","arguments":"{\"cmd\":\"pwd\"}"},{"type":"function_call_output","call_id":"call_1","output":"E:/grok2api2"},{"type":"reasoning","summary":[]},{"type":"function_call","call_id":"call_2","name":"bash","arguments":"{\"cmd\":\"pwd\"}"},{"type":"function_call_output","call_id":"call_2","output":"E:/grok2api2"},{"type":"reasoning","summary":[]},{"type":"function_call","call_id":"call_3","name":"bash","arguments":"{\"cmd\":\"pwd\"}"},{"type":"function_call_output","call_id":"call_3","output":"E:/grok2api2"}]}`)

	got, evidence := appendContinueForTrailingToolLoop(body)
	if !evidence.Detected {
		t.Fatalf("reasoning items must not break an otherwise repeated tool tail: %#v", evidence)
	}
	assertTrailingContinue(t, got)
}

func TestAppendContinueForTrailingToolLoopRequiresCompletedTail(t *testing.T) {
	t.Parallel()
	body := []byte(`{"model":"grok-4.7","input":[{"type":"message","role":"user","content":"inspect"},{"type":"function_call","call_id":"call_1","name":"bash","arguments":"{\"cmd\":\"pwd\"}"},{"type":"function_call_output","call_id":"call_1","output":"E:/grok2api2"},{"type":"function_call","call_id":"call_2","name":"bash","arguments":"{\"cmd\":\"pwd\"}"},{"type":"function_call_output","call_id":"call_2","output":"E:/grok2api2"},{"type":"function_call","call_id":"call_3","name":"bash","arguments":"{\"cmd\":\"pwd\"}"},{"type":"function_call_output","call_id":"call_3","output":"E:/grok2api2"},{"type":"function_call","call_id":"call_4","name":"bash","arguments":"{\"cmd\":\"pwd\"}"}]}`)

	got, evidence := appendContinueForTrailingToolLoop(body)
	if evidence.Detected {
		t.Fatalf("an incomplete trailing call must not reuse an earlier hit: %#v", evidence)
	}
	if string(got) != string(body) {
		t.Fatalf("body changed with an incomplete trailing call: %s", got)
	}
}

func assertTrailingContinue(t *testing.T, body []byte) {
	t.Helper()
	var root struct {
		Input []struct {
			Type    string `json:"type"`
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"input"`
	}
	if err := json.Unmarshal(body, &root); err != nil {
		t.Fatal(err)
	}
	if len(root.Input) == 0 {
		t.Fatal("missing input")
	}
	last := root.Input[len(root.Input)-1]
	if last.Type != "message" || last.Role != "user" || last.Content != toolLoopBreakoutInstruction {
		t.Fatalf("trailing input = %#v, want user continue", last)
	}
}

func TestAppendContinueForTrailingToolLoopIgnoresDynamicRunnerHeaders(t *testing.T) {
	t.Parallel()
	body := []byte(`{"model":"grok-4.7","input":[{"type":"message","role":"user","content":"edit file"},{"type":"function_call","call_id":"call_1","name":"exec_command","arguments":"{\"cmd\":\"python patch.py\"}"},{"type":"function_call_output","call_id":"call_1","output":"Chunk ID: 1a2b3c\nWall time: 0.1413 seconds\nProcess exited with code 1\nOriginal token count: 41\nOutput:\nSyntaxError: unterminated string" },{"type":"function_call","call_id":"call_2","name":"exec_command","arguments":"{\"cmd\":\"python patch.py\"}"},{"type":"function_call_output","call_id":"call_2","output":"Chunk ID: 4d5e6f\nWall time: 0.1629 seconds\nProcess exited with code 1\nOriginal token count: 41\nOutput:\nSyntaxError: unterminated string" },{"type":"function_call","call_id":"call_3","name":"exec_command","arguments":"{\"cmd\":\"python patch.py\"}"},{"type":"function_call_output","call_id":"call_3","output":"Chunk ID: 7a8b9c\nWall time: 0.1758 seconds\nProcess exited with code 1\nOriginal token count: 41\nOutput:\nSyntaxError: unterminated string" }]}`)

	got, evidence := appendContinueForTrailingToolLoop(body)
	if !evidence.Detected {
		t.Fatal("three identical calls with varying Chunk ID/Wall time headers must still be detected as a loop")
	}
	if evidence.ConsecutiveCount != 3 {
		t.Fatalf("consecutive count = %d, want 3", evidence.ConsecutiveCount)
	}
	assertTrailingContinue(t, got)
}

func TestAppendContinueForTrailingToolLoopDetectsAlternatingCycleWithAssistantMessages(t *testing.T) {
	t.Parallel()
	body := []byte(`{"model":"grok-4.7","input":[{"type":"message","role":"user","content":"inspect the binary"},{"type":"function_call","call_id":"call_1","name":"exec_command","arguments":"{\"cmd\":\"inspect timeout\"}"},{"type":"function_call_output","call_id":"call_1","output":"timeout result"},{"type":"function_call","call_id":"call_2","name":"exec_command","arguments":"{\"cmd\":\"inspect q6o\"}"},{"type":"message","role":"assistant","content":"I will inspect it again."},{"type":"function_call_output","call_id":"call_2","output":"q6o result"},{"type":"function_call","call_id":"call_3","name":"exec_command","arguments":"{\"cmd\":\"inspect timeout\"}"},{"type":"function_call_output","call_id":"call_3","output":"timeout result"},{"type":"function_call","call_id":"call_4","name":"exec_command","arguments":"{\"cmd\":\"inspect q6o\"}"},{"type":"message","role":"assistant","content":"I will inspect it again."},{"type":"function_call_output","call_id":"call_4","output":"q6o result"},{"type":"function_call","call_id":"call_5","name":"exec_command","arguments":"{\"cmd\":\"inspect timeout\"}"},{"type":"function_call_output","call_id":"call_5","output":"timeout result"},{"type":"function_call","call_id":"call_6","name":"exec_command","arguments":"{\"cmd\":\"inspect q6o\"}"},{"type":"message","role":"assistant","content":"I will inspect it again."},{"type":"function_call_output","call_id":"call_6","output":"q6o result"}]}`)

	got, evidence := appendContinueForTrailingToolLoop(body)
	if !evidence.Detected {
		t.Fatal("three identical A-B tool cycles separated by assistant messages must be detected")
	}
	if evidence.ConsecutiveCount != 6 {
		t.Fatalf("consecutive count = %d, want 6 completed calls", evidence.ConsecutiveCount)
	}
	assertTrailingContinue(t, got)
}

func TestAppendContinueForTrailingToolLoopDoesNotTreatDifferentCallsAsCycle(t *testing.T) {
	t.Parallel()
	body := []byte(`{"model":"grok-4.7","input":[{"type":"message","role":"user","content":"inspect"},{"type":"function_call","call_id":"call_1","name":"bash","arguments":"{\"cmd\":\"pwd\"}"},{"type":"function_call_output","call_id":"call_1","output":"E:/grok2api2"},{"type":"message","role":"assistant","content":"next"},{"type":"function_call","call_id":"call_2","name":"bash","arguments":"{\"cmd\":\"git status\"}"},{"type":"function_call_output","call_id":"call_2","output":" M service.go"},{"type":"function_call","call_id":"call_3","name":"bash","arguments":"{\"cmd\":\"git diff\"}"},{"type":"function_call_output","call_id":"call_3","output":"diff --git a/service.go b/service.go"}]}`)

	got, evidence := appendContinueForTrailingToolLoop(body)
	if evidence.Detected {
		t.Fatalf("three distinct calls must not be detected as a loop: %#v", evidence)
	}
	if string(got) != string(body) {
		t.Fatalf("body changed without a loop: %s", got)
	}
}
