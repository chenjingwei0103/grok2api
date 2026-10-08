package media

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	mediaapp "github.com/chenyme/grok2api/backend/internal/application/media"
	localmedia "github.com/chenyme/grok2api/backend/internal/infra/media"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/infra/upstreamcapture"
	"github.com/gin-gonic/gin"
)

type importResolverFunc func(ctx context.Context, network, host string) ([]netip.Addr, error)

func (fn importResolverFunc) LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error) {
	return fn(ctx, network, host)
}

func TestPublicIngestAddressPolicy(t *testing.T) {
	blocked := []string{
		"127.0.0.1", "10.0.0.1", "169.254.169.254", "100.64.0.1", "0.0.0.1",
		"192.0.2.1", "198.18.0.1", "240.0.0.1", "::1", "fd00::1", "2001:db8::1",
	}
	for _, raw := range blocked {
		if isPublicIP(netip.MustParseAddr(raw)) {
			t.Errorf("address %s was allowed", raw)
		}
	}
	for _, raw := range []string{"8.8.8.8", "1.1.1.1", "2606:4700:4700::1111"} {
		if !isPublicIP(netip.MustParseAddr(raw)) {
			t.Errorf("public address %s was blocked", raw)
		}
	}
}

func TestValidRedirectURLRejectsCredentialsAndUnexpectedPorts(t *testing.T) {
	for _, raw := range []string{"file:///tmp/a.png", "https://user:pass@example.com/a.png", "https://example.com:8443/a.png"} {
		request, err := http.NewRequest(http.MethodGet, raw, nil)
		if err == nil && isValidRedirectURL(request.URL) {
			t.Errorf("URL %q was allowed", raw)
		}
	}
	request, err := http.NewRequest(http.MethodGet, "https://example.com/a.png", nil)
	if err != nil || !isValidRedirectURL(request.URL) {
		t.Fatalf("public HTTPS URL rejected: %v", err)
	}
}

func TestResolveImportTargetPinsValidatedPublicAddress(t *testing.T) {
	parsed, err := url.Parse("https://Images.Example.test/photo.png?size=large#ignored")
	if err != nil {
		t.Fatal(err)
	}
	resolver := importResolverFunc(func(_ context.Context, network, host string) ([]netip.Addr, error) {
		if network != "ip" || host != "images.example.test" {
			t.Fatalf("lookup network=%q host=%q", network, host)
		}
		return []netip.Addr{netip.MustParseAddr("93.184.216.34")}, nil
	})
	target, err := resolveImportTarget(context.Background(), parsed, resolver)
	if err != nil {
		t.Fatal(err)
	}
	if target.fetchURL.Host != "93.184.216.34:443" || target.fetchURL.Path != "/photo.png" || target.fetchURL.RawQuery != "size=large" || target.fetchURL.Fragment != "" {
		t.Fatalf("fetch URL = %s", target.fetchURL)
	}
	if target.hostHeader != "Images.Example.test" || target.serverName != "images.example.test" {
		t.Fatalf("host header=%q server name=%q", target.hostHeader, target.serverName)
	}
	client, transport := newIngestHTTPClient(target)
	defer transport.CloseIdleConnections()
	if client.CheckRedirect == nil || transport.TLSClientConfig == nil || transport.TLSClientConfig.ServerName != "images.example.test" || transport.TLSClientConfig.MinVersion != tls.VersionTLS12 {
		t.Fatal("pinned HTTPS client did not preserve strict TLS verification")
	}
}

func TestResolveImportTargetRejectsAnyNonPublicDNSAnswer(t *testing.T) {
	parsed, err := url.Parse("https://images.example.test/photo.png")
	if err != nil {
		t.Fatal(err)
	}
	resolver := importResolverFunc(func(context.Context, string, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("93.184.216.34"), netip.MustParseAddr("127.0.0.1")}, nil
	})
	if _, err := resolveImportTarget(context.Background(), parsed, resolver); !errors.Is(err, errFetchBlocked) {
		t.Fatalf("mixed public/private DNS answer was not blocked: %v", err)
	}
}

func TestResolveImportTargetDoesNotClassifyDNSFailureAsPolicyBlock(t *testing.T) {
	parsed, err := url.Parse("https://missing.example.test/photo.png")
	if err != nil {
		t.Fatal(err)
	}
	lookupErr := errors.New("temporary DNS failure")
	resolver := importResolverFunc(func(context.Context, string, string) ([]netip.Addr, error) {
		return nil, lookupErr
	})
	_, err = resolveImportTarget(context.Background(), parsed, resolver)
	if !errors.Is(err, lookupErr) || errors.Is(err, errFetchBlocked) {
		t.Fatalf("DNS failure classification = %v", err)
	}
}

func TestAdminUploadCreatesHiddenTransientInput(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "media-ingest.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	objects, err := localmedia.NewLocalStore(filepath.Join(t.TempDir(), "objects"))
	if err != nil {
		t.Fatal(err)
	}
	service := mediaapp.NewService(relational.NewMediaAssetRepository(database), relational.NewMediaJobRepository(database), objects, nil, mediaapp.Config{
		PublicBaseURL: "https://api.example", MaxImageBytes: 32 << 20, MaxTotalBytes: 1 << 30,
		CleanupThresholdPercent: 80, CleanupInterval: time.Minute,
	})
	raw, _ := base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII=")
	var requestBody bytes.Buffer
	writer := multipart.NewWriter(&requestBody)
	part, err := writer.CreateFormFile("file", "input.png")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(raw); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}

	handler := NewHandler(service)
	router := gin.New()
	handler.RegisterPublic(router)
	handler.RegisterAdmin(router.Group("/api/admin/v1"))
	request := httptest.NewRequest(http.MethodPost, "/api/admin/v1/media/inputs/upload", &requestBody)
	request.Header.Set("Content-Type", writer.FormDataContentType())
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("upload status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var envelope struct {
		Data struct {
			FileID    string `json:"fileId"`
			ExpiresAt string `json:"expiresAt"`
		} `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(envelope.Data.FileID, "input_") || envelope.Data.ExpiresAt == "" {
		t.Fatalf("response=%s", recorder.Body.String())
	}
	public := httptest.NewRecorder()
	router.ServeHTTP(public, httptest.NewRequest(http.MethodGet, "/v1/media/images/"+envelope.Data.FileID, nil))
	if public.Code != http.StatusNotFound {
		t.Fatalf("transient input became public: status=%d", public.Code)
	}
	if values, total, err := service.AdminListImages(ctx, 1, 20, ""); err != nil || total != 0 || len(values) != 0 {
		t.Fatalf("gallery values=%#v total=%d err=%v", values, total, err)
	}
}

func TestIngestClientCapturesRemoteFetchWhenEnabled(t *testing.T) {
	captureDirectory := t.TempDir()
	t.Setenv(upstreamcapture.EnvironmentDirectory, captureDirectory)
	payload := []byte("image-bytes")
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "image/png")
		_, _ = writer.Write(payload)
	}))
	defer server.Close()

	parsed, err := url.Parse(server.URL + "/photo.png")
	if err != nil {
		t.Fatal(err)
	}
	client, inner := newIngestHTTPClient(&importTarget{fetchURL: parsed, hostHeader: parsed.Host, serverName: "images.example.test"})
	inner.DialContext = (&net.Dialer{Timeout: 2 * time.Second}).DialContext
	request, err := http.NewRequest(http.MethodGet, parsed.String(), nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if err := response.Body.Close(); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(data, payload) {
		t.Fatalf("ingest body = %q", data)
	}
	bundle := singleIngestCaptureBundle(t, captureDirectory)
	for _, name := range []string{"request.json", "request.body", "response.json", "response.body", "result.json"} {
		if _, err := os.Stat(filepath.Join(bundle, name)); err != nil {
			t.Fatalf("missing %s: %v", name, err)
		}
	}
	if captured, err := os.ReadFile(filepath.Join(bundle, "response.body")); err != nil || !bytes.Equal(captured, payload) {
		t.Fatalf("captured response = %q, err = %v", captured, err)
	}

	t.Setenv(upstreamcapture.EnvironmentDirectory, "")
	disabledRequest, err := http.NewRequest(http.MethodGet, parsed.String(), nil)
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
	entries, err := os.ReadDir(captureDirectory)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("capture entries after disable = %d, want 1", len(entries))
	}
}

func singleIngestCaptureBundle(t *testing.T, captureDirectory string) string {
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
