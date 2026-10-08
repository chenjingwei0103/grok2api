package upstreamcapture

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func TestWrapTransportCapturesCompleteHTTPExchange(t *testing.T) {
	captureDirectory := t.TempDir()
	t.Setenv(EnvironmentDirectory, captureDirectory)

	client := &http.Client{Transport: WrapTransport(roundTripFunc(func(request *http.Request) (*http.Response, error) {
		body, err := io.ReadAll(request.Body)
		if err != nil {
			t.Fatalf("read request body: %v", err)
		}
		if string(body) != "request payload" {
			t.Fatalf("upstream request body = %q", body)
		}
		return &http.Response{
			StatusCode: http.StatusCreated,
			Status:     "201 Created",
			Header:     http.Header{"X-Upstream-Trace": {"trace-1"}},
			Body:       io.NopCloser(bytes.NewBufferString("response payload")),
			Request:    request,
		}, nil
	}))}
	request, err := http.NewRequest(http.MethodPost, "https://upstream.example/v1/videos?debug=1", bytes.NewBufferString("request payload"))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer local-debug-secret")
	request.Header.Set("Content-Type", "application/json")

	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	responseBody, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if err := response.Body.Close(); err != nil {
		t.Fatal(err)
	}
	if string(responseBody) != "response payload" {
		t.Fatalf("response body = %q", responseBody)
	}

	entries, err := os.ReadDir(captureDirectory)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || !entries[0].IsDir() {
		t.Fatalf("capture entries = %#v, want one directory", entries)
	}
	bundle := filepath.Join(captureDirectory, entries[0].Name())
	if data, err := os.ReadFile(filepath.Join(bundle, "request.body")); err != nil || string(data) != "request payload" {
		t.Fatalf("request body evidence = %q, err = %v", data, err)
	}
	if data, err := os.ReadFile(filepath.Join(bundle, "response.body")); err != nil || string(data) != "response payload" {
		t.Fatalf("response body evidence = %q, err = %v", data, err)
	}

	requestManifest := readCaptureJSON(t, filepath.Join(bundle, "request.json"))
	if requestManifest["method"] != http.MethodPost || requestManifest["url"] != "https://upstream.example/v1/videos?debug=1" {
		t.Fatalf("request manifest = %#v", requestManifest)
	}
	headers, ok := requestManifest["headers"].(map[string]any)
	if !ok || headers["Authorization"] == nil {
		t.Fatalf("request headers = %#v", requestManifest["headers"])
	}

	responseManifest := readCaptureJSON(t, filepath.Join(bundle, "response.json"))
	if responseManifest["status_code"] != float64(http.StatusCreated) || responseManifest["status"] != "201 Created" {
		t.Fatalf("response manifest = %#v", responseManifest)
	}
	result := readCaptureJSON(t, filepath.Join(bundle, "result.json"))
	if result["response_body_bytes"] != float64(len("response payload")) || result["transport_error"] != nil {
		t.Fatalf("result = %#v", result)
	}
}

func TestWrapTransportWritesNothingWhenDisabled(t *testing.T) {
	captureDirectory := t.TempDir()
	t.Setenv(EnvironmentDirectory, captureDirectory)
	client := &http.Client{Transport: WrapTransport(roundTripFunc(func(request *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Status: "200 OK", Body: io.NopCloser(bytes.NewBufferString("one")), Request: request}, nil
	}))}
	request, err := http.NewRequest(http.MethodGet, "https://upstream.example/once", nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadAll(response.Body); err != nil {
		t.Fatal(err)
	}
	if err := response.Body.Close(); err != nil {
		t.Fatal(err)
	}
	enabledEntries, err := os.ReadDir(captureDirectory)
	if err != nil {
		t.Fatal(err)
	}
	if len(enabledEntries) != 1 {
		t.Fatalf("enabled capture entries = %d, want 1", len(enabledEntries))
	}

	t.Setenv(EnvironmentDirectory, "")
	disabledRequest, err := http.NewRequest(http.MethodGet, "https://upstream.example/disabled", nil)
	if err != nil {
		t.Fatal(err)
	}
	disabledResponse, err := client.Do(disabledRequest)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadAll(disabledResponse.Body); err != nil {
		t.Fatal(err)
	}
	if err := disabledResponse.Body.Close(); err != nil {
		t.Fatal(err)
	}
	if session := Start(disabledRequest); session != nil {
		t.Fatal("Start created a capture session while the directory was unset")
	}
	disabledEntries, err := os.ReadDir(captureDirectory)
	if err != nil {
		t.Fatal(err)
	}
	if len(disabledEntries) != 1 {
		t.Fatalf("disabled capture entries = %d, want the single enabled bundle", len(disabledEntries))
	}
}

func readCaptureJSON(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	value := map[string]any{}
	if err := json.Unmarshal(data, &value); err != nil {
		t.Fatal(err)
	}
	return value
}
