package gateway

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// conversationCaptureConfig is intentionally local-only. A bundle contains the
// original user request and upstream stream, so it must be explicitly enabled.
type conversationCaptureConfig struct {
	Enabled   bool
	Directory string
}

type conversationCaptureManifest struct {
	Kind       string    `json:"kind"`
	RequestID  string    `json:"requestId"`
	Method     string    `json:"method"`
	Path       string    `json:"path"`
	CreatedAt  time.Time `json:"createdAt"`
	Request    string    `json:"request"`
	Response   string    `json:"response"`
	RequestSHA string    `json:"requestSha256"`
}

func startConversationCapture(config conversationCaptureConfig, requestID, method, path string, body []byte) (string, func(error)) {
	if !config.Enabled || len(body) == 0 || strings.TrimSpace(config.Directory) == "" {
		return "", func(error) {}
	}
	safeID := sanitizeCaptureFilePart(requestID)
	if safeID == "" {
		safeID = "request"
	}
	directory := filepath.Join(config.Directory, fmt.Sprintf("%s-%s", time.Now().UTC().Format("20060102T150405.000000000Z"), safeID))
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return "", func(error) {}
	}
	requestPath := filepath.Join(directory, "request.json")
	if err := os.WriteFile(requestPath, body, 0o600); err != nil {
		_ = os.RemoveAll(directory)
		return "", func(error) {}
	}
	digest := sha256.Sum256(body)
	manifest := conversationCaptureManifest{
		Kind: "grok2api_conversation_capture_v1", RequestID: requestID,
		Method: method, Path: sanitizeRequestPath(path), CreatedAt: time.Now().UTC(),
		Request: "request.json", Response: "upstream.sse", RequestSHA: fmt.Sprintf("%x", digest[:]),
	}
	encoded, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil || os.WriteFile(filepath.Join(directory, "manifest.json"), encoded, 0o600) != nil {
		_ = os.RemoveAll(directory)
		return "", func(error) {}
	}
	return directory, func(readErr error) {
		status := map[string]any{"completedAt": time.Now().UTC().Format(time.RFC3339Nano)}
		if readErr != nil && readErr != io.EOF {
			status["streamError"] = sanitizeDiagnosticText(readErr.Error(), 2048)
		}
		encoded, err := json.MarshalIndent(status, "", "  ")
		if err == nil {
			_ = os.WriteFile(filepath.Join(directory, "result.json"), encoded, 0o600)
		}
	}
}

func sanitizeCaptureFilePart(value string) string {
	value = strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			return r
		default:
			return -1
		}
	}, value)
	return strings.Trim(value, "-_")
}

type conversationCaptureReadCloser struct {
	source io.ReadCloser
	file   *os.File
	finish func(error)
	once   sync.Once
}

func newConversationCaptureReadCloser(source io.ReadCloser, directory string, finish func(error)) io.ReadCloser {
	if source == nil || directory == "" {
		return source
	}
	file, err := os.OpenFile(filepath.Join(directory, "upstream.sse"), os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return source
	}
	return &conversationCaptureReadCloser{source: source, file: file, finish: finish}
}

func (r *conversationCaptureReadCloser) Read(dst []byte) (int, error) {
	n, err := r.source.Read(dst)
	if n > 0 && r.file != nil {
		if _, writeErr := r.file.Write(dst[:n]); writeErr != nil {
			r.finishOnce(writeErr)
		}
	}
	if err != nil {
		r.finishOnce(err)
	}
	return n, err
}

func (r *conversationCaptureReadCloser) Close() error {
	r.finishOnce(nil)
	return r.source.Close()
}

func (r *conversationCaptureReadCloser) finishOnce(err error) {
	r.once.Do(func() {
		if r.file != nil {
			_ = r.file.Close()
		}
		if r.finish != nil {
			r.finish(err)
		}
	})
}
