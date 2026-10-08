// Package upstreamcapture provides an opt-in, local-only HTTP exchange
// recorder for diagnosing upstream behavior. It is intentionally disabled
// unless GROK2API_UPSTREAM_CAPTURE_DIR is set because the bundles preserve raw
// request and response data, including credentials and user content.
package upstreamcapture

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// EnvironmentDirectory explicitly enables raw upstream HTTP capture. The
// caller is responsible for using a local, access-controlled directory.
const EnvironmentDirectory = "GROK2API_UPSTREAM_CAPTURE_DIR"

const (
	captureRequestManifest  = "request.json"
	captureRequestBody      = "request.body"
	captureResponseManifest = "response.json"
	captureResponseBody     = "response.body"
	captureResult           = "result.json"
)

type requestManifest struct {
	Kind          string      `json:"kind"`
	CreatedAt     string      `json:"created_at"`
	Method        string      `json:"method"`
	URL           string      `json:"url"`
	Host          string      `json:"host,omitempty"`
	ContentLength int64       `json:"content_length"`
	Headers       http.Header `json:"headers"`
	BodyFile      string      `json:"body_file"`
}

type responseManifest struct {
	StatusCode     int         `json:"status_code"`
	Status         string      `json:"status"`
	ContentLength  int64       `json:"content_length"`
	Headers        http.Header `json:"headers,omitempty"`
	BodyFile       string      `json:"body_file"`
	TransportError string      `json:"transport_error,omitempty"`
}

type resultManifest struct {
	CompletedAt          string `json:"completed_at"`
	RequestBodyBytes     int64  `json:"request_body_bytes"`
	ResponseBodyBytes    int64  `json:"response_body_bytes"`
	RequestCaptureError  string `json:"request_capture_error,omitempty"`
	ResponseCaptureError string `json:"response_capture_error,omitempty"`
	TransportError       string `json:"transport_error,omitempty"`
}

// Session owns one raw local capture bundle. Start returns nil whenever the
// opt-in environment variable is empty or its local directory cannot be used;
// capture failures must never alter an upstream request's behavior.
type Session struct {
	directory string

	mu                   sync.Mutex
	requestBytes         int64
	responseBytes        int64
	requestCaptureError  string
	responseCaptureError string
	transportError       string
	responseFinished     bool
}

// Start creates a capture bundle and installs a streaming request-body tee on
// request. It records bytes as the actual transport consumes them instead of
// buffering large image/video uploads in memory.
func Start(request *http.Request) *Session {
	if request == nil {
		return nil
	}
	root := strings.TrimSpace(os.Getenv(EnvironmentDirectory))
	if root == "" {
		return nil
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil
	}
	directory, err := os.MkdirTemp(root, "upstream-")
	if err != nil {
		return nil
	}
	session := &Session{directory: directory}
	if err := writeJSON(filepath.Join(directory, captureRequestManifest), requestManifest{
		Kind:          "grok2api_upstream_http_capture_v1",
		CreatedAt:     time.Now().UTC().Format(time.RFC3339Nano),
		Method:        request.Method,
		URL:           request.URL.String(),
		Host:          request.Host,
		ContentLength: request.ContentLength,
		Headers:       request.Header.Clone(),
		BodyFile:      captureRequestBody,
	}); err != nil {
		_ = os.RemoveAll(directory)
		return nil
	}
	requestFile, err := os.OpenFile(filepath.Join(directory, captureRequestBody), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		_ = os.RemoveAll(directory)
		return nil
	}
	if request.Body == nil {
		_ = requestFile.Close()
		return session
	}
	request.Body = newCaptureReadCloser(request.Body, requestFile, session.finishRequestBody)
	return session
}

// Finish writes response headers and wraps the response body so that content
// is persisted as downstream code reads it. It returns the original response
// with only Body replaced by an equivalent recording wrapper.
func (s *Session) Finish(response *http.Response, transportErr error) *http.Response {
	if s == nil {
		return response
	}
	if transportErr != nil {
		s.mu.Lock()
		s.transportError = truncateError(transportErr)
		s.mu.Unlock()
	}
	if response == nil {
		_ = writeJSON(filepath.Join(s.directory, captureResponseManifest), responseManifest{BodyFile: captureResponseBody, TransportError: s.transportErrorValue()})
		_ = os.WriteFile(filepath.Join(s.directory, captureResponseBody), nil, 0o600)
		s.finishResponseBody(0, "")
		return nil
	}
	_ = writeJSON(filepath.Join(s.directory, captureResponseManifest), responseManifest{
		StatusCode:     response.StatusCode,
		Status:         response.Status,
		ContentLength:  response.ContentLength,
		Headers:        response.Header.Clone(),
		BodyFile:       captureResponseBody,
		TransportError: s.transportErrorValue(),
	})
	responseFile, err := os.OpenFile(filepath.Join(s.directory, captureResponseBody), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		s.finishResponseBody(0, err.Error())
		return response
	}
	if response.Body == nil {
		_ = responseFile.Close()
		s.finishResponseBody(0, "")
		return response
	}
	response.Body = newCaptureReadCloser(response.Body, responseFile, s.finishResponseBody)
	return response
}

// WrapTransport makes capture available to standard net/http clients that do
// not use an egress Lease. A nil transport preserves net/http's default
// transport behavior.
//
// WebSocket dialers (Grok web gateway, Imagine, and Console voice) are not
// net/http RoundTrippers and stay uncovered. Non-Grok clients — GitHub update
// checks, proxy-subscription fetches, local FlareSolverr, egress connectivity
// probes, and the tunnel proxy — are also left unwrapped.
func WrapTransport(base http.RoundTripper) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	return captureTransport{base: base}
}

type captureTransport struct {
	base http.RoundTripper
}

func (t captureTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	session := Start(request)
	response, err := t.base.RoundTrip(request)
	if session != nil {
		response = session.Finish(response, err)
	}
	return response, err
}

func (s *Session) finishRequestBody(bytes int64, captureErr string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requestBytes = bytes
	if captureErr != "" && s.requestCaptureError == "" {
		s.requestCaptureError = captureErr
	}
}

func (s *Session) finishResponseBody(bytes int64, captureErr string) {
	s.mu.Lock()
	if s.responseFinished {
		s.mu.Unlock()
		return
	}
	s.responseFinished = true
	s.responseBytes = bytes
	if captureErr != "" {
		s.responseCaptureError = captureErr
	}
	result := resultManifest{
		CompletedAt:          time.Now().UTC().Format(time.RFC3339Nano),
		RequestBodyBytes:     s.requestBytes,
		ResponseBodyBytes:    s.responseBytes,
		RequestCaptureError:  s.requestCaptureError,
		ResponseCaptureError: s.responseCaptureError,
		TransportError:       s.transportError,
	}
	s.mu.Unlock()
	_ = writeJSON(filepath.Join(s.directory, captureResult), result)
}

func (s *Session) transportErrorValue() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.transportError
}

type captureReadCloser struct {
	source   io.ReadCloser
	file     *os.File
	finished func(int64, string)

	mu    sync.Mutex
	bytes int64
	once  sync.Once
}

func newCaptureReadCloser(source io.ReadCloser, file *os.File, finished func(int64, string)) *captureReadCloser {
	return &captureReadCloser{source: source, file: file, finished: finished}
}

func (r *captureReadCloser) Read(buffer []byte) (int, error) {
	count, readErr := r.source.Read(buffer)
	if count > 0 {
		r.mu.Lock()
		r.bytes += int64(count)
		var writeErr error
		if r.file != nil {
			_, writeErr = r.file.Write(buffer[:count])
		}
		r.mu.Unlock()
		if writeErr != nil {
			r.finish(writeErr.Error())
		}
	}
	if readErr != nil {
		if readErr == io.EOF {
			r.finish("")
		} else {
			r.finish(readErr.Error())
		}
	}
	return count, readErr
}

func (r *captureReadCloser) Close() error {
	err := r.source.Close()
	if err != nil {
		r.finish(err.Error())
	} else {
		r.finish("")
	}
	return err
}

func (r *captureReadCloser) finish(captureErr string) {
	r.once.Do(func() {
		r.mu.Lock()
		bytes := r.bytes
		file := r.file
		r.file = nil
		r.mu.Unlock()
		if file != nil {
			if err := file.Close(); err != nil && captureErr == "" {
				captureErr = err.Error()
			}
		}
		if r.finished != nil {
			r.finished(bytes, captureErr)
		}
	})
}

func writeJSON(path string, value any) error {
	encoded, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, encoded, 0o600)
}

func truncateError(err error) string {
	if err == nil {
		return ""
	}
	value := err.Error()
	const maxBytes = 4096
	if len(value) > maxBytes {
		return value[:maxBytes]
	}
	return value
}
