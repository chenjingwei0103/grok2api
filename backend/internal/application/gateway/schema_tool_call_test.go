package gateway

import (
	"bytes"
	"encoding/json"
	"testing"
)

func TestSchemaIndexArguments(t *testing.T) {
	t.Parallel()
	if !schemaIndexArguments([]byte(`{"kind":1,"mode":2,"name":3,"prompt":4,"rruleSchedule":5,"status":6}`)) {
		t.Fatal("expected schema index object")
	}
	if !schemaIndexArguments([]byte(`"{\"name\":1,\"prompt\":2,\"rruleSchedule\":3,\"status\":4,\"mode\":5}"`)) {
		t.Fatal("expected quoted schema index arguments")
	}
	for _, raw := range []string{
		`{}`,
		`{"cmd":"rg"}`,
		`{"limit":1,"offset":2}`,
		`{"a":1,"b":2,"c":3}`,
		`{"mode":"heartbeat","name":"watch","prompt":"check","status":"active"}`,
		`{"a":1,"b":2,"c":3,"d":5}`,
		`{"a":0,"b":1,"c":2,"d":3}`,
	} {
		if schemaIndexArguments([]byte(raw)) {
			t.Fatalf("false positive: %s", raw)
		}
	}
}

func TestStripSchemaIndexToolCallsKeepsRealCalls(t *testing.T) {
	t.Parallel()
	body := []byte(`{"model":"grok-4.7","input":[{"type":"message","role":"user","content":"hi"},{"type":"function_call","call_id":"bad","name":"automation_update","arguments":"{\"kind\":1,\"mode\":2,\"name\":3,\"prompt\":4,\"rruleSchedule\":5,\"status\":6}"},{"type":"function_call_output","call_id":"bad","output":""},{"type":"function_call","call_id":"good","name":"exec_command","arguments":"{\"cmd\":\"rg\"}"},{"type":"function_call","call_id":"real","name":"automation_update","arguments":"{\"mode\":\"heartbeat\",\"name\":\"watch\",\"prompt\":\"check\",\"rruleSchedule\":\"FREQ=HOURLY\",\"status\":\"active\"}"}]}`)
	cleaned, removed := stripSchemaIndexToolCalls(body)
	if removed != 2 {
		t.Fatalf("removed = %d, want 2", removed)
	}
	var root struct {
		Model string `json:"model"`
		Input []struct {
			Type   string `json:"type"`
			CallID string `json:"call_id"`
		} `json:"input"`
	}
	if err := json.Unmarshal(cleaned, &root); err != nil {
		t.Fatal(err)
	}
	if root.Model != "grok-4.7" {
		t.Fatalf("model = %s", root.Model)
	}
	got := make([]string, 0, len(root.Input))
	for _, item := range root.Input {
		got = append(got, item.Type+":"+item.CallID)
	}
	want := []string{"message:", "function_call:good", "function_call:real"}
	if len(got) != len(want) {
		t.Fatalf("items = %#v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("items = %#v", got)
		}
	}
	if bytes.Contains(cleaned, []byte(`"call_id":"bad"`)) {
		t.Fatal("bad call remained")
	}
}

func TestClassifySchemaIndexToolCallWithholdsWithoutDisablingToolCalls(t *testing.T) {
	t.Parallel()
	schema := QualityStreamSignals{Terminal: true, ToolCallOnly: true, SchemaIndexToolCall: true, OutputTokens: 40}
	if got := classifyQualityHoldWithSpeed(schema, 8, 1000); got != QualityWithhold {
		t.Fatalf("schema tool call = %s", got)
	}
	normal := QualityStreamSignals{Terminal: true, ToolCallOnly: true, OutputTokens: 40, OutputTokensPerSecond: 1200}
	if got := classifyQualityHoldWithSpeed(normal, 8, 1000); got != QualityDeliver {
		t.Fatalf("normal tool call = %s", got)
	}
	if qualityRetryNeedsReasoningAfterRetry(schema, QualityWithhold) {
		t.Fatal("schema tool call must not force later reasoning")
	}
}
