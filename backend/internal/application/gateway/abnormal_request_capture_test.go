package gateway

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAbnormalRequestFileCaptureWritesOriginalBody(t *testing.T) {
	directory := t.TempDir()
	body := []byte(`{"model":"grok-4.7","input":[{"role":"user","content":"replay me"}]}`)

	path, err := writeAbnormalRequestFile(abnormalRequestFileCaptureConfig{
		Enabled:   true,
		Directory: directory,
	}, body)
	if err != nil {
		t.Fatalf("write abnormal request: %v", err)
	}
	if filepath.Dir(path) != directory {
		t.Fatalf("capture directory = %q, want %q", filepath.Dir(path), directory)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read capture: %v", err)
	}
	if string(got) != string(body) {
		t.Fatalf("captured body = %q, want %q", got, body)
	}
}

func TestAbnormalRequestFileCaptureSkipsWhenDisabled(t *testing.T) {
	directory := t.TempDir()

	path, err := writeAbnormalRequestFile(abnormalRequestFileCaptureConfig{
		Directory: directory,
	}, []byte(`{"model":"grok-4.7"}`))
	if err != nil {
		t.Fatalf("write disabled capture: %v", err)
	}
	if path != "" {
		t.Fatalf("capture path = %q, want empty", path)
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatalf("read directory: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("capture entries = %d, want 0", len(entries))
	}
}
