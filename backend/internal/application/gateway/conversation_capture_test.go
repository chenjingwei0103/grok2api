package gateway

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestConversationCaptureWritesReplayBundle(t *testing.T) {
	directory := t.TempDir()
	request := []byte(`{"model":"grok-4.7","input":"local replay"}`)
	bundle, finish := startConversationCapture(conversationCaptureConfig{
		Enabled: true, Directory: directory,
	}, "req_capture_1", "POST", "/v1/responses?ignored=1", request)
	if bundle == "" {
		t.Fatal("capture bundle was not created")
	}
	body := newConversationCaptureReadCloser(io.NopCloser(strings.NewReader("data: hello\n\ndata: [DONE]\n\n")), bundle, finish)
	received, err := io.ReadAll(body)
	if err != nil {
		t.Fatal(err)
	}
	if err := body.Close(); err != nil {
		t.Fatal(err)
	}
	capturedRequest, err := os.ReadFile(filepath.Join(bundle, "request.json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(capturedRequest) != string(request) {
		t.Fatalf("request = %q, want %q", capturedRequest, request)
	}
	capturedResponse, err := os.ReadFile(filepath.Join(bundle, "upstream.sse"))
	if err != nil {
		t.Fatal(err)
	}
	if string(capturedResponse) != string(received) {
		t.Fatalf("response = %q, want %q", capturedResponse, received)
	}
	manifest, err := os.ReadFile(filepath.Join(bundle, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(manifest), `"path": "/v1/responses"`) {
		t.Fatalf("manifest path was not sanitized: %s", manifest)
	}
	if _, err := os.Stat(filepath.Join(bundle, "result.json")); err != nil {
		t.Fatalf("missing result file: %v", err)
	}
}

func TestConversationCaptureDisabledDoesNotWrite(t *testing.T) {
	directory := t.TempDir()
	bundle, finish := startConversationCapture(conversationCaptureConfig{Directory: directory}, "req", "POST", "/v1/responses", []byte(`{}`))
	finish(nil)
	if bundle != "" {
		t.Fatalf("disabled capture directory = %q", bundle)
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("disabled capture wrote %d entries", len(entries))
	}
}
