package gateway

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/png"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
)

func TestEvaluateVideoQualityRejectsWrongOrientation(t *testing.T) {
	err := evaluateVideoQuality(videoQualityProbe{
		Width:  1280,
		Height: 720,
		Frames: []videoImageHash{{}},
	}, videoQualitySpec{AspectRatio: "9:16", ReferenceMode: true}, nil)
	if err == nil {
		t.Fatal("expected a 16:9 output to fail a 9:16 request")
	}
}

func TestSaveVideoWithQualityPreservesRejectedEvidence(t *testing.T) {
	guardDir := t.TempDir()
	t.Setenv("GROK2API_QUALITY_GUARD_DIR", guardDir)

	var logOutput bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logOutput, &slog.HandlerOptions{Level: slog.LevelInfo}))
	store := &videoAssetStoreStub{}
	service := &Service{
		mediaAssets: store,
		logger:      logger,
		videoQualityInspector: videoQualityInspectorFunc(func(context.Context, string) (videoQualityProbe, error) {
			return videoQualityProbe{Width: 1280, Height: 720}, nil
		}),
	}
	const jobID = "video_job"
	const signedURL = "https://cdn.example/video.mp4?token=super-secret-token"
	payload := []byte("video")
	traceCtx := withVideoUpstreamTrace(context.Background(), &videoUpstreamTrace{
		GenerationAttempt: 2,
		AccountID:         17,
		Provider:          "grok_web",
		Model:             "grok-video",
		InputMode:         "reference",
		ReferenceCount:    1,
		Resolution:        "720p",
		ResultURL:         signedURL,
	})
	_, err := service.saveVideoWithQuality(
		traceCtx,
		jobID,
		"video/mp4",
		bytes.NewReader(payload),
		videoQualitySpec{AspectRatio: "9:16", ReferenceMode: true},
	)
	var qualityErr *videoQualityError
	if !errors.As(err, &qualityErr) {
		t.Fatalf("save video error = %v, want videoQualityError", err)
	}
	if store.saveCalls != 0 {
		t.Fatalf("rejected video SaveVideo calls = %d, want 0", store.saveCalls)
	}

	evidenceDirs, globErr := filepath.Glob(filepath.Join(guardDir, "video-evidence", jobID, "*"))
	if globErr != nil {
		t.Fatal(globErr)
	}
	if len(evidenceDirs) != 1 {
		t.Fatalf("evidence directories = %v, want exactly one", evidenceDirs)
	}

	videoBytes, readVideoErr := os.ReadFile(filepath.Join(evidenceDirs[0], "result.mp4"))
	if readVideoErr != nil {
		t.Fatalf("read rejected video evidence: %v", readVideoErr)
	}
	if !bytes.Equal(videoBytes, payload) {
		t.Fatalf("rejected video evidence = %q, want %q", videoBytes, payload)
	}

	manifestBytes, readManifestErr := os.ReadFile(filepath.Join(evidenceDirs[0], "manifest.json"))
	if readManifestErr != nil {
		t.Fatalf("read rejected video manifest: %v", readManifestErr)
	}
	var manifest struct {
		JobID                string `json:"job_id"`
		Reason               string `json:"reason"`
		RequestedAspectRatio string `json:"requested_aspect_ratio"`
		ActualWidth          int    `json:"actual_width"`
		ActualHeight         int    `json:"actual_height"`
		ReferenceMode        bool   `json:"reference_mode"`
	}
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		t.Fatalf("decode rejected video manifest: %v", err)
	}
	if manifest.JobID != jobID || manifest.Reason == "" || manifest.RequestedAspectRatio != "9:16" || manifest.ActualWidth != 1280 || manifest.ActualHeight != 720 || !manifest.ReferenceMode {
		t.Fatalf("manifest = %#v", manifest)
	}
	manifestLower := strings.ToLower(string(manifestBytes))
	for _, forbidden := range []string{"data:image", "prompt", "api_key", "token"} {
		if strings.Contains(manifestLower, forbidden) {
			t.Fatalf("manifest must not contain sensitive request content %q: %s", forbidden, manifestBytes)
		}
	}

	logText := logOutput.String()
	if strings.Contains(logText, signedURL) || strings.Contains(logText, "super-secret-token") || strings.Contains(logText, "https://") {
		t.Fatalf("quality log leaked the result URL: %s", logText)
	}
	records := videoUpstreamLogRecords(t, logText)
	rejection := videoUpstreamLogByMessage(records, "video_quality_rejected")
	if rejection == nil {
		t.Fatalf("quality logs = %#v", records)
	}
	if rejection["quality_evidence_dir"] != evidenceDirs[0] || rejection["quality_rejection_reason"] == "" {
		t.Fatalf("quality rejection log = %#v", rejection)
	}
	if rejection["job_id"] != jobID || rejection["generation_attempt"] != float64(2) || rejection["account_id"] != float64(17) {
		t.Fatalf("quality rejection identity = %#v", rejection)
	}
	if rejection["provider"] != "grok_web" || rejection["model"] != "grok-video" || rejection["input_mode"] != "reference" || rejection["reference_count"] != float64(1) {
		t.Fatalf("quality rejection request fields = %#v", rejection)
	}
	if rejection["resolution"] != "720p" || rejection["requested_aspect_ratio"] != "9:16" || rejection["actual_width"] != float64(1280) || rejection["actual_height"] != float64(720) {
		t.Fatalf("quality rejection dimensions = %#v", rejection)
	}
	if rejection["result_url_present"] != true || rejection["result_url_host"] != "cdn.example" || rejection["result_url_has_query"] != true || rejection["result_url_fingerprint"] == "" {
		t.Fatalf("quality rejection URL metadata = %#v", rejection)
	}
	if rejection["frame_similarity_summary"] != "frames=0" || rejection["actual_cached_bytes"] != float64(len(payload)) {
		t.Fatalf("quality rejection frame summary = %#v", rejection)
	}
}

func TestPersistRemoteVideoLogsSavedDownloadWithoutRawURL(t *testing.T) {
	var logOutput bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logOutput, &slog.HandlerOptions{Level: slog.LevelInfo}))
	adapter := &videoPersistAdapter{}
	store := &videoAssetStoreStub{}
	service := &Service{mediaAssets: store, logger: logger}
	const signedURL = "https://assets.grok.com/video.mp4?token=download-secret"
	result, err := service.persistRemoteVideo(
		context.Background(),
		"video_job",
		adapter,
		account.Credential{ID: 42, Provider: account.ProviderWeb},
		provider.VideoResult{URL: signedURL, ContentType: "video/mp4"},
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.AssetID != "vid_local" || store.saveCalls != 1 {
		t.Fatalf("result = %#v saveCalls = %d", result, store.saveCalls)
	}
	logText := logOutput.String()
	if strings.Contains(logText, signedURL) || strings.Contains(logText, "download-secret") || strings.Contains(logText, "https://") {
		t.Fatalf("download log leaked the result URL: %s", logText)
	}
	saved := videoUpstreamLogByMessage(videoUpstreamLogRecords(t, logText), "video_download_saved")
	if saved == nil {
		t.Fatalf("download logs = %s", logText)
	}
	if saved["job_id"] != "video_job" || saved["account_id"] != float64(42) || saved["provider"] != string(account.ProviderWeb) {
		t.Fatalf("saved download identity = %#v", saved)
	}
	if saved["result_url_present"] != true || saved["result_url_host"] != "assets.grok.com" || saved["result_url_has_query"] != true || saved["result_url_fingerprint"] == "" {
		t.Fatalf("saved download URL metadata = %#v", saved)
	}
	if saved["result_asset_id_present"] != true || saved["download_attempt"] != float64(1) || saved["download_content_type"] != "video/mp4" || saved["download_declared_bytes"] != float64(5) {
		t.Fatalf("saved download body metadata = %#v", saved)
	}
	if saved["actual_cached_bytes"] != float64(len("video")) {
		t.Fatalf("saved download size = %#v", saved)
	}
}

func TestPersistRemoteVideoLogsDownloadFailureWithoutRawURL(t *testing.T) {
	var logOutput bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logOutput, &slog.HandlerOptions{Level: slog.LevelInfo}))
	adapter := &videoPersistAdapter{failures: videoOutputAttempts}
	service := &Service{mediaAssets: &videoAssetStoreStub{}, logger: logger}
	const signedURL = "https://assets.grok.com/video.mp4?token=download-secret"
	_, err := service.persistRemoteVideo(
		context.Background(),
		"video_job",
		adapter,
		account.Credential{ID: 7, Provider: account.ProviderWeb},
		provider.VideoResult{URL: signedURL},
	)
	if err == nil {
		t.Fatal("expected download failure")
	}
	logText := logOutput.String()
	if strings.Contains(logText, signedURL) || strings.Contains(logText, "download-secret") || strings.Contains(logText, "https://") {
		t.Fatalf("download failure log leaked the result URL: %s", logText)
	}
	failed := videoUpstreamLogByMessage(videoUpstreamLogRecords(t, logText), "video_download_failed")
	if failed == nil {
		t.Fatalf("download failure logs = %s", logText)
	}
	if failed["job_id"] != "video_job" || failed["account_id"] != float64(7) || failed["download_attempt"] != float64(1) {
		t.Fatalf("download failure log = %#v", failed)
	}
	if failed["result_url_present"] != true || failed["result_url_host"] != "assets.grok.com" || failed["result_url_fingerprint"] == "" {
		t.Fatalf("download failure URL metadata = %#v", failed)
	}
}

func videoUpstreamLogRecords(t *testing.T, output string) []map[string]any {
	t.Helper()
	records := make([]map[string]any, 0)
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("decode log line %q: %v", line, err)
		}
		records = append(records, record)
	}
	return records
}

func videoUpstreamLogByMessage(records []map[string]any, message string) map[string]any {
	for _, record := range records {
		if record["msg"] == message {
			return record
		}
	}
	return nil
}

func TestEvaluateVideoQualityRejectsReferenceWithNoSimilarFrame(t *testing.T) {
	source := solidVideoQualityImage(color.RGBA{R: 240, G: 30, B: 30, A: 255})
	other := solidVideoQualityImage(color.RGBA{R: 30, G: 30, B: 240, A: 255})
	sourceHash, err := hashVideoQualityImage(source)
	if err != nil {
		t.Fatal(err)
	}
	otherHash, err := hashVideoQualityImage(other)
	if err != nil {
		t.Fatal(err)
	}
	err = evaluateVideoQuality(videoQualityProbe{
		Width:  640,
		Height: 1408,
		Frames: []videoImageHash{otherHash, otherHash, otherHash},
	}, videoQualitySpec{AspectRatio: "9:16", ReferenceMode: true}, []videoImageHash{sourceHash})
	if err == nil || !strings.Contains(err.Error(), "参考图主体未出现在抽帧中") {
		t.Fatalf("reference result with no similar frame must reject, got %v", err)
	}
}

func TestEvaluateVideoQualityAcceptsOneMatchingReferenceFrame(t *testing.T) {
	source := solidVideoQualityImage(color.RGBA{R: 240, G: 30, B: 30, A: 255})
	other := solidVideoQualityImage(color.RGBA{R: 30, G: 30, B: 240, A: 255})
	sourceHash, err := hashVideoQualityImage(source)
	if err != nil {
		t.Fatal(err)
	}
	otherHash, err := hashVideoQualityImage(other)
	if err != nil {
		t.Fatal(err)
	}
	if err := evaluateVideoQuality(videoQualityProbe{
		Width:  640,
		Height: 1408,
		Frames: []videoImageHash{otherHash, otherHash, sourceHash},
	}, videoQualitySpec{AspectRatio: "9:16", ReferenceMode: true}, []videoImageHash{sourceHash}); err != nil {
		t.Fatalf("expected one matching reference frame to pass: %v", err)
	}
}

func TestEvaluateVideoQualityAcceptsTwoMatchingReferenceFrames(t *testing.T) {
	source := solidVideoQualityImage(color.RGBA{R: 240, G: 30, B: 30, A: 255})
	other := solidVideoQualityImage(color.RGBA{R: 30, G: 30, B: 240, A: 255})
	sourceHash, err := hashVideoQualityImage(source)
	if err != nil {
		t.Fatal(err)
	}
	otherHash, err := hashVideoQualityImage(other)
	if err != nil {
		t.Fatal(err)
	}
	if err := evaluateVideoQuality(videoQualityProbe{
		Width:  640,
		Height: 1408,
		Frames: []videoImageHash{sourceHash, otherHash, sourceHash},
	}, videoQualitySpec{AspectRatio: "9:16", ReferenceMode: true}, []videoImageHash{sourceHash}); err != nil {
		t.Fatalf("expected two matching frames to pass: %v", err)
	}
}

func TestEvaluateVideoQualityRejectsFirstFrameMismatch(t *testing.T) {
	source := solidVideoQualityImage(color.RGBA{R: 240, G: 30, B: 30, A: 255})
	other := solidVideoQualityImage(color.RGBA{R: 30, G: 30, B: 240, A: 255})
	sourceHash, err := hashVideoQualityImage(source)
	if err != nil {
		t.Fatal(err)
	}
	otherHash, err := hashVideoQualityImage(other)
	if err != nil {
		t.Fatal(err)
	}
	err = evaluateVideoQuality(videoQualityProbe{
		Width:  640,
		Height: 1408,
		Frames: []videoImageHash{otherHash, sourceHash, sourceHash},
	}, videoQualitySpec{AspectRatio: "9:16", FirstFrameMode: true}, []videoImageHash{sourceHash})
	if err == nil || !strings.Contains(err.Error(), "首帧未继承输入图片") {
		t.Fatalf("first-frame mismatch must reject, got %v", err)
	}
}

func TestVideoQualityCanSwitchAccountExactlySixTimesAfterInitialResult(t *testing.T) {
	for qualityFailures := 1; qualityFailures <= 6; qualityFailures++ {
		if !videoQualityCanSwitchAccount(qualityFailures) {
			t.Fatalf("quality failure %d must still switch to a new account", qualityFailures)
		}
	}
	if videoQualityCanSwitchAccount(7) {
		t.Fatal("the seventh rejected result must stop instead of starting an eighth account")
	}
}

func TestNewVideoQualitySpecHashesDataURLInput(t *testing.T) {
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, solidVideoQualityImage(color.RGBA{R: 240, G: 30, B: 30, A: 255})); err != nil {
		t.Fatal(err)
	}
	spec := newVideoQualitySpec("9:16", "data:image/png;base64,"+base64.StdEncoding.EncodeToString(encoded.Bytes()), nil)
	if !spec.FirstFrameMode || spec.ReferenceMode || len(spec.referenceHashes) != 1 {
		t.Fatalf("quality spec = %#v", spec)
	}
}

func TestParseFFmpegVideoQualityMetadata(t *testing.T) {
	probe, duration, err := parseFFmpegVideoQualityMetadata([]byte(`
Input #0, mov,mp4,m4a,3gp,3g2,mj2, from 'sample.mp4':
  Duration: 00:00:06.083, start: 0.000000, bitrate: 2150 kb/s
  Stream #0:0[0x1](und): Video: h264 (High), yuv420p(progressive), 720x1280, 2000 kb/s, 30 fps, 30 tbr
`))
	if err != nil {
		t.Fatal(err)
	}
	if probe.Width != 720 || probe.Height != 1280 {
		t.Fatalf("probe dimensions = %dx%d, want 720x1280", probe.Width, probe.Height)
	}
	if duration != 6.083 {
		t.Fatalf("duration = %v, want 6.083", duration)
	}
}

func solidVideoQualityImage(value color.Color) image.Image {
	imageValue := image.NewRGBA(image.Rect(0, 0, 32, 32))
	for y := 0; y < 32; y++ {
		for x := 0; x < 32; x++ {
			imageValue.Set(x, y, value)
		}
	}
	return imageValue
}
