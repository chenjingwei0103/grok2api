package egress

import (
	"bytes"
	"crypto/tls"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

type captureRoundTripFunc func(*http.Request) (*http.Response, error)

func (f captureRoundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func TestLeaseDoCapturesRawUpstreamExchangeWhenEnabled(t *testing.T) {
	captureDirectory := t.TempDir()
	t.Setenv("GROK2API_UPSTREAM_CAPTURE_DIR", captureDirectory)
	client := &scriptedRequestClient{do: func(_ int, request *http.Request) (*http.Response, error) {
		body, err := io.ReadAll(request.Body)
		if err != nil {
			t.Fatalf("read lease request body: %v", err)
		}
		if string(body) != "request payload" {
			t.Fatalf("lease request body = %q", body)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Header:     http.Header{"X-Upstream": {"capture-test"}},
			Body:       io.NopCloser(bytes.NewBufferString("response payload")),
			Request:    request,
		}, nil
	}}
	lease := &Lease{client: client}
	request, err := http.NewRequest(http.MethodPost, "https://upstream.example/generate", bytes.NewBufferString("request payload"))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer local-debug-secret")

	response, err := lease.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadAll(response.Body); err != nil {
		t.Fatal(err)
	}
	if err := response.Body.Close(); err != nil {
		t.Fatal(err)
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
		t.Fatalf("request evidence = %q, err = %v", data, err)
	}
	if data, err := os.ReadFile(filepath.Join(bundle, "response.body")); err != nil || string(data) != "response payload" {
		t.Fatalf("response evidence = %q, err = %v", data, err)
	}
	for _, name := range []string{"request.json", "request.body", "response.json", "response.body", "result.json"} {
		if _, err := os.Stat(filepath.Join(bundle, name)); err != nil {
			t.Fatalf("missing %s: %v", name, err)
		}
	}
}

func TestLeaseDoPinnedHTTPSCapturesExchangeWhenEnabled(t *testing.T) {
	captureDirectory := t.TempDir()
	t.Setenv("GROK2API_UPSTREAM_CAPTURE_DIR", captureDirectory)
	previous := pinnedHTTPSClientFactory
	pinnedHTTPSClientFactory = func(_, _ string, _ *tls.Config) (*http.Client, error) {
		return &http.Client{Transport: captureRoundTripFunc(func(request *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Status:     "200 OK",
				Header:     http.Header{"Content-Type": {"image/png"}},
				Body:       io.NopCloser(bytes.NewBufferString("png-bytes")),
				Request:    request,
			}, nil
		})}, nil
	}
	t.Cleanup(func() { pinnedHTTPSClientFactory = previous })

	request, err := http.NewRequest(http.MethodGet, "https://8.8.8.8:443/image.png?debug=1", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Host = "images.example.test"
	response, err := (&Lease{}).DoPinnedHTTPS(request, "images.example.test")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadAll(response.Body); err != nil {
		t.Fatal(err)
	}
	if err := response.Body.Close(); err != nil {
		t.Fatal(err)
	}

	bundle := singleCaptureBundle(t, captureDirectory)
	for _, name := range []string{"request.json", "request.body", "response.json", "response.body", "result.json"} {
		if _, err := os.Stat(filepath.Join(bundle, name)); err != nil {
			t.Fatalf("missing %s: %v", name, err)
		}
	}
	if data, err := os.ReadFile(filepath.Join(bundle, "response.body")); err != nil || string(data) != "png-bytes" {
		t.Fatalf("pinned response evidence = %q, err = %v", data, err)
	}

	t.Setenv("GROK2API_UPSTREAM_CAPTURE_DIR", "")
	disabledRequest, err := http.NewRequest(http.MethodGet, "https://8.8.8.8:443/image.png", nil)
	if err != nil {
		t.Fatal(err)
	}
	disabledRequest.Host = "images.example.test"
	disabledResponse, err := (&Lease{}).DoPinnedHTTPS(disabledRequest, "images.example.test")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadAll(disabledResponse.Body); err != nil {
		t.Fatal(err)
	}
	if err := disabledResponse.Body.Close(); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(captureDirectory)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("capture entries after disable = %d, want 1", len(entries))
	}
}

func singleCaptureBundle(t *testing.T, captureDirectory string) string {
	t.Helper()
	entries, err := os.ReadDir(captureDirectory)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || !entries[0].IsDir() {
		t.Fatalf("capture entries = %#v, want one directory", entries)
	}
	return filepath.Join(captureDirectory, entries[0].Name())
}
