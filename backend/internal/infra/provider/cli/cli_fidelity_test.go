package cli

import (
	"bytes"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"
)

func readUpstreamRequestBody(t *testing.T, request *http.Request) []byte {
	t.Helper()
	data, err := io.ReadAll(request.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.EqualFold(strings.TrimSpace(request.Header.Get("Content-Encoding")), "zstd") {
		return data
	}
	decoder, err := zstd.NewReader(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer decoder.Close()
	decoded, err := decoder.DecodeAll(data, nil)
	if err != nil {
		t.Fatal(err)
	}
	return decoded
}

func TestGrokConversationGroupIDMatchesBuildDerivation(t *testing.T) {
	const root = "0f4c7a1e-6c1b-5a0e-8e6c-3b0f6a6a2a1d"
	if got := grokConversationGroupID(root); got != "d5de345a-3509-59bc-b73c-7d33029d840b" {
		t.Fatalf("conversation group id = %q", got)
	}
	if grokConversationGroupID("  ") != "" {
		t.Fatal("blank session must not get a group id")
	}
}

func TestGrokTurnIndexForRequest(t *testing.T) {
	body := []byte(`{"input":[{"type":"message","role":"user"},{"type":"message","role":"assistant"},{"role":"user","content":"again"}]}`)
	if got := grokTurnIndexForRequest("7", "session", body); got != "7" {
		t.Fatalf("explicit turn = %q", got)
	}
	if got := grokTurnIndexForRequest("", "", body); got != "" {
		t.Fatalf("stateless turn = %q", got)
	}
	if got := grokTurnIndexForRequest("", "session", body); got != "2" {
		t.Fatalf("estimated turn = %q", got)
	}
	if got := estimateGrokTurnIndex([]byte(`{"messages":[{"role":"user"},{"role":"user"}]}`)); got != 2 {
		t.Fatalf("message turn = %d", got)
	}
}

func TestCompressBuildPlaneRequest(t *testing.T) {
	if compressed, encoding := compressBuildPlaneRequest("build", []byte("small")); encoding != "" || string(compressed) != "small" {
		t.Fatalf("small body compressed: %q %q", encoding, compressed)
	}
	if _, encoding := compressBuildPlaneRequest("xai", bytes.Repeat([]byte("a"), buildRequestCompressionMinBytes+1)); encoding != "" {
		t.Fatalf("xai plane encoding = %q", encoding)
	}
	body := bytes.Repeat([]byte("prompt cache prefix "), buildRequestCompressionMinBytes/8)
	compressed, encoding := compressBuildPlaneRequest("build", body)
	if encoding != "zstd" || len(compressed) == 0 || len(compressed) >= len(body) {
		t.Fatalf("encoding=%q size %d -> %d", encoding, len(body), len(compressed))
	}
	decoder, err := zstd.NewReader(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer decoder.Close()
	decoded, err := decoder.DecodeAll(compressed, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(decoded, body) {
		t.Fatal("zstd round trip mismatch")
	}
}
