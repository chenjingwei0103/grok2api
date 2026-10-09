package gateway

import (
	"encoding/json"
	"testing"
)

func TestStripTrailingSingleQuestionMarks(t *testing.T) {
	t.Parallel()

	// Test 1: trailing question mark is removed
	body := []byte(`{"model":"grok-4.7","input":[{"type":"message","role":"user","content":"hello"},{"type":"message","role":"assistant","content":"hi"},{"type":"message","role":"user","content":"?"}]}`)
	cleaned, removed := stripTrailingSingleQuestionMarks(body)
	if removed != 1 {
		t.Fatalf("expected removed=1, got %d", removed)
	}
	var root struct {
		Input []map[string]any `json:"input"`
	}
	_ = json.Unmarshal(cleaned, &root)
	if len(root.Input) != 2 {
		t.Fatalf("expected 2 items, got %d", len(root.Input))
	}

	// Test 2: multiple trailing question marks removed
	bodyMulti := []byte(`{"model":"grok-4.7","input":[{"type":"message","role":"user","content":"do task"},{"type":"message","role":"assistant","content":"working"},{"type":"message","role":"user","content":"？"},{"type":"message","role":"user","content":"?"}]}`)
	cleanedMulti, removedMulti := stripTrailingSingleQuestionMarks(bodyMulti)
	if removedMulti != 2 {
		t.Fatalf("expected removed=2, got %d", removedMulti)
	}
	_ = json.Unmarshal(cleanedMulti, &root)
	if len(root.Input) != 2 {
		t.Fatalf("expected 2 items, got %d", len(root.Input))
	}

	// Test 3: normal single question user message is kept if it is the only message
	bodyOnly := []byte(`{"model":"grok-4.7","input":[{"type":"message","role":"user","content":"?"}]}`)
	cleanedOnly, removedOnly := stripTrailingSingleQuestionMarks(bodyOnly)
	if removedOnly != 0 {
		t.Fatalf("single only message should not be stripped, got %d", removedOnly)
	}
	if string(cleanedOnly) != string(bodyOnly) {
		t.Fatalf("body should not change")
	}

	// Test 4: normal user message with actual text is kept
	bodyNormal := []byte(`{"model":"grok-4.7","input":[{"type":"message","role":"user","content":"what is this?"}]}`)
	_, removedNormal := stripTrailingSingleQuestionMarks(bodyNormal)
	if removedNormal != 0 {
		t.Fatalf("expected removed=0, got %d", removedNormal)
	}
}
