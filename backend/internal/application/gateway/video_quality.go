package gateway

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"math"
	"math/bits"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	mediadomain "github.com/chenyme/grok2api/backend/internal/domain/media"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
)

// videoQualityAccountSwitches is the number of replacement accounts allowed
// after the first completed upstream video is rejected by the local quality
// probe. The initial account is not counted here, so a job can use at most
// seven accounts for this specific failure mode.
const videoQualityAccountSwitches = 6

const videoQualityMaxVideoBytes = 256 << 20

const (
	videoQualityEvidenceDirectory    = "video-evidence"
	videoQualityEvidenceVideoFile    = "result.mp4"
	videoQualityEvidenceManifestFile = "manifest.json"
)

type videoQualitySpec struct {
	AspectRatio     string
	ReferenceMode   bool
	FirstFrameMode  bool
	referenceHashes []videoImageHash
}

type videoQualityProbe struct {
	Width           int
	Height          int
	Frames          []videoImageHash
	FrameProbeError string
}

type videoQualityInspector interface {
	Inspect(context.Context, string) (videoQualityProbe, error)
}

type videoQualityInspectorFunc func(context.Context, string) (videoQualityProbe, error)

func (f videoQualityInspectorFunc) Inspect(ctx context.Context, path string) (videoQualityProbe, error) {
	return f(ctx, path)
}

// videoImageHash is deliberately small enough to keep quality diagnostics
// metadata-only. It combines a luminance aHash with average RGB so uniform
// but differently coloured frames do not collapse to the same aHash.
type videoImageHash struct {
	luminance uint64
	red       uint8
	green     uint8
	blue      uint8
}

type videoQualityError struct {
	reason string
}

// videoQualityEvidenceManifest deliberately contains only metadata needed to
// reproduce the local quality verdict. It must never include request prompts,
// input data URLs, upstream URLs, credentials, or account secrets.
type videoQualityEvidenceManifest struct {
	CollectedAt          string                      `json:"collected_at"`
	JobID                string                      `json:"job_id"`
	Reason               string                      `json:"reason"`
	RequestedAspectRatio string                      `json:"requested_aspect_ratio"`
	ActualWidth          int                         `json:"actual_width"`
	ActualHeight         int                         `json:"actual_height"`
	FirstFrameMode       bool                        `json:"first_frame_mode"`
	ReferenceMode        bool                        `json:"reference_mode"`
	ReferenceImageCount  int                         `json:"reference_image_count"`
	FrameProbeError      string                      `json:"frame_probe_error,omitempty"`
	Frames               []videoQualityFrameEvidence `json:"frames,omitempty"`
}

type videoQualityFrameEvidence struct {
	Index                    int  `json:"index"`
	MatchesReference         bool `json:"matches_reference"`
	MinimumHammingDistance   int  `json:"minimum_hamming_distance"`
	MinimumAverageColorDelta int  `json:"minimum_average_color_delta"`
}

func (e *videoQualityError) Error() string {
	if e == nil || strings.TrimSpace(e.reason) == "" {
		return "视频生成结果未通过质量校验"
	}
	return "视频生成结果未通过质量校验: " + e.reason
}

func isVideoQualityError(err error) bool {
	var qualityErr *videoQualityError
	return errors.As(err, &qualityErr)
}

func videoQualityCanSwitchAccount(qualityFailures int) bool {
	return qualityFailures > 0 && qualityFailures <= videoQualityAccountSwitches
}

func newVideoQualitySpec(aspectRatio, imageURL string, referenceURLs []string) videoQualitySpec {
	spec := videoQualitySpec{
		AspectRatio:    strings.TrimSpace(aspectRatio),
		FirstFrameMode: strings.TrimSpace(imageURL) != "",
		ReferenceMode:  len(referenceURLs) > 0,
	}
	inputs := make([]string, 0, 1+len(referenceURLs))
	if value := strings.TrimSpace(imageURL); value != "" {
		inputs = append(inputs, value)
	}
	for _, raw := range referenceURLs {
		if value := strings.TrimSpace(raw); value != "" {
			inputs = append(inputs, value)
		}
	}
	spec.referenceHashes = hashVideoQualityDataURLInputs(inputs)
	return spec
}

func (s videoQualitySpec) enabled() bool {
	return requestedVideoOrientation(s.AspectRatio) != 0 || len(s.referenceHashes) > 0
}

func hashVideoQualityDataURLInputs(values []string) []videoImageHash {
	hashes := make([]videoImageHash, 0, len(values))
	for _, raw := range values {
		imageValue, err := decodeVideoQualityDataURL(raw)
		if err != nil {
			continue
		}
		hash, err := hashVideoQualityImage(imageValue)
		if err == nil {
			hashes = append(hashes, hash)
		}
	}
	return hashes
}

func decodeVideoQualityDataURL(raw string) (image.Image, error) {
	raw = strings.TrimSpace(raw)
	if !strings.HasPrefix(strings.ToLower(raw), "data:") {
		return nil, fmt.Errorf("input is not a data URL")
	}
	separator := strings.IndexByte(raw, ',')
	if separator < 0 || !strings.Contains(strings.ToLower(raw[:separator]), ";base64") {
		return nil, fmt.Errorf("input data URL is not base64")
	}
	data, err := base64.StdEncoding.DecodeString(raw[separator+1:])
	if err != nil {
		return nil, fmt.Errorf("decode input data URL: %w", err)
	}
	decoded, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("decode input image: %w", err)
	}
	return decoded, nil
}

func (s *Service) saveVideoWithQuality(ctx context.Context, jobID, contentType string, body io.Reader, spec videoQualitySpec) (mediadomain.Asset, error) {
	if !spec.enabled() {
		return s.mediaAssets.SaveVideo(ctx, jobID, contentType, body)
	}
	temporary, err := os.CreateTemp("", "grok2api-video-quality-*.mp4")
	if err != nil {
		return mediadomain.Asset{}, fmt.Errorf("创建视频质量临时文件: %w", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	written, copyErr := io.Copy(temporary, io.LimitReader(body, int64(videoQualityMaxVideoBytes)+1))
	closeErr := temporary.Close()
	if copyErr != nil {
		return mediadomain.Asset{}, fmt.Errorf("缓存视频质量检测内容: %w", copyErr)
	}
	if closeErr != nil {
		return mediadomain.Asset{}, fmt.Errorf("关闭视频质量临时文件: %w", closeErr)
	}
	if written > int64(videoQualityMaxVideoBytes) {
		return mediadomain.Asset{}, fmt.Errorf("视频内容超过最大归档大小")
	}

	probe, inspectErr := s.inspectVideoQuality(ctx, temporaryPath)
	if inspectErr != nil {
		if s.logger != nil {
			s.logger.Warn("video_quality_probe_skipped", "job_id", jobID, "error", inspectErr)
		}
	} else {
		if probe.FrameProbeError != "" && s.logger != nil {
			s.logger.Warn("video_quality_frame_probe_skipped", "job_id", jobID, "error", probe.FrameProbeError)
		}
		if qualityErr := evaluateVideoQuality(probe, spec, spec.referenceHashes); qualityErr != nil {
			evidenceDirectory, evidenceErr := saveVideoQualityEvidence(temporaryPath, jobID, spec, probe, qualityErr)
			if evidenceErr != nil && s.logger != nil {
				s.logger.Warn("video_quality_evidence_save_failed", "job_id", jobID, "reason", qualityErr.reason, "error", evidenceErr)
			}
			s.logVideoQualityRejection(ctx, jobID, spec, probe, written, qualityErr, evidenceDirectory)
			return mediadomain.Asset{}, qualityErr
		}
		if trace := videoUpstreamTraceFrom(ctx); trace != nil {
			trace.ActualWidth = probe.Width
			trace.ActualHeight = probe.Height
			trace.ActualCachedBytes = written
			if trace.FrameSimilaritySummary == "" {
				trace.FrameSimilaritySummary = formatVideoFrameSimilarity(probe, spec)
			}
		}
	}

	reader, err := os.Open(temporaryPath)
	if err != nil {
		return mediadomain.Asset{}, fmt.Errorf("重新打开视频质量临时文件: %w", err)
	}
	defer reader.Close()
	return s.mediaAssets.SaveVideo(ctx, jobID, contentType, reader)
}

// videoUpstreamTrace carries non-secret fields used to cluster video results.
// ResultURL is an input to the fingerprint only and is never written to a log.
type videoUpstreamTrace struct {
	JobID                  string
	GenerationAttempt      int
	AccountID              uint64
	Provider               string
	Model                  string
	InputMode              string
	ReferenceCount         int
	Resolution             string
	RequestedAspectRatio   string
	ResultURL              string
	ResultAssetID          string
	DownloadAttempt        int
	DownloadDeclaredBytes  int64
	DownloadContentType    string
	ActualCachedBytes      int64
	ActualWidth            int
	ActualHeight           int
	FrameSimilaritySummary string
	QualityRejectionReason string
	QualityEvidenceDir     string
}

type videoUpstreamTraceContextKey struct{}

func withVideoUpstreamTrace(ctx context.Context, trace *videoUpstreamTrace) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if trace == nil {
		return ctx
	}
	return context.WithValue(ctx, videoUpstreamTraceContextKey{}, trace)
}

func videoUpstreamTraceFrom(ctx context.Context) *videoUpstreamTrace {
	if ctx == nil {
		return nil
	}
	trace, _ := ctx.Value(videoUpstreamTraceContextKey{}).(*videoUpstreamTrace)
	return trace
}

func videoInputMode(imageURL string, referenceURLs []string) string {
	if countVideoReferences(referenceURLs) > 0 {
		return "reference"
	}
	if strings.TrimSpace(imageURL) != "" {
		return "first_frame"
	}
	return "text"
}

func countVideoReferences(referenceURLs []string) int {
	count := 0
	for _, raw := range referenceURLs {
		if strings.TrimSpace(raw) != "" {
			count++
		}
	}
	return count
}

func videoQualityInputMode(spec videoQualitySpec) string {
	if spec.ReferenceMode {
		return "reference"
	}
	if spec.FirstFrameMode {
		return "first_frame"
	}
	return "text"
}

func (s *Service) bindVideoDownloadTrace(ctx context.Context, jobID string, credential account.Credential, result provider.VideoResult, spec videoQualitySpec) (context.Context, *videoUpstreamTrace) {
	trace := videoUpstreamTraceFrom(ctx)
	if trace == nil {
		trace = &videoUpstreamTrace{}
		ctx = withVideoUpstreamTrace(ctx, trace)
	}
	if trace.JobID == "" {
		trace.JobID = jobID
	}
	if trace.AccountID == 0 && credential.ID > 0 {
		trace.AccountID = credential.ID
	}
	if trace.Provider == "" && credential.Provider != "" {
		trace.Provider = string(credential.Provider)
	}
	if trace.RequestedAspectRatio == "" {
		trace.RequestedAspectRatio = spec.AspectRatio
	}
	if trace.InputMode == "" {
		trace.InputMode = videoQualityInputMode(spec)
	}
	trace.ResultURL = result.URL
	if result.AssetID != "" {
		trace.ResultAssetID = result.AssetID
	}
	return ctx, trace
}

func (s *Service) logVideoQualityRejection(ctx context.Context, jobID string, spec videoQualitySpec, probe videoQualityProbe, cachedBytes int64, qualityErr *videoQualityError, evidenceDirectory string) {
	trace := videoUpstreamTraceFrom(ctx)
	if trace == nil {
		trace = &videoUpstreamTrace{}
	}
	if trace.JobID == "" {
		trace.JobID = jobID
	}
	if trace.RequestedAspectRatio == "" {
		trace.RequestedAspectRatio = spec.AspectRatio
	}
	if trace.InputMode == "" {
		trace.InputMode = videoQualityInputMode(spec)
	}
	if trace.ReferenceCount == 0 {
		trace.ReferenceCount = len(spec.referenceHashes)
	}
	trace.ActualCachedBytes = cachedBytes
	trace.ActualWidth = probe.Width
	trace.ActualHeight = probe.Height
	trace.FrameSimilaritySummary = formatVideoFrameSimilarity(probe, spec)
	if qualityErr != nil {
		trace.QualityRejectionReason = qualityErr.reason
	}
	trace.QualityEvidenceDir = evidenceDirectory
	s.logVideoUpstream("video_quality_rejected", trace)
}

func (s *Service) logVideoUpstream(event string, trace *videoUpstreamTrace) {
	if s == nil || s.logger == nil || trace == nil || event == "" {
		return
	}
	attributes := videoUpstreamLogAttrs(trace)
	switch event {
	case "video_quality_rejected", "video_download_failed", "video_generation_attempt_failed":
		s.logger.Warn(event, attributes...)
	default:
		s.logger.Info(event, attributes...)
	}
}

func videoUpstreamLogAttrs(trace *videoUpstreamTrace) []any {
	attributes := []any{
		"job_id", trace.JobID,
		"generation_attempt", trace.GenerationAttempt,
		"reference_count", trace.ReferenceCount,
		"result_asset_id_present", trace.ResultAssetID != "",
	}
	if trace.AccountID > 0 {
		attributes = append(attributes, "account_id", trace.AccountID)
	}
	if trace.Provider != "" {
		attributes = append(attributes, "provider", trace.Provider)
	}
	if trace.Model != "" {
		attributes = append(attributes, "model", trace.Model)
	}
	if trace.InputMode != "" {
		attributes = append(attributes, "input_mode", trace.InputMode)
	}
	if trace.Resolution != "" {
		attributes = append(attributes, "resolution", trace.Resolution)
	}
	if trace.RequestedAspectRatio != "" {
		attributes = append(attributes, "requested_aspect_ratio", trace.RequestedAspectRatio)
	}
	attributes = append(attributes, videoResultURLAttrs(trace.ResultURL)...)
	if trace.DownloadAttempt > 0 {
		attributes = append(attributes,
			"download_attempt", trace.DownloadAttempt,
			"download_declared_bytes", trace.DownloadDeclaredBytes,
		)
	}
	if trace.DownloadContentType != "" {
		attributes = append(attributes, "download_content_type", trace.DownloadContentType)
	}
	if trace.ActualCachedBytes > 0 {
		attributes = append(attributes, "actual_cached_bytes", trace.ActualCachedBytes)
	}
	if trace.ActualWidth > 0 || trace.ActualHeight > 0 {
		attributes = append(attributes, "actual_width", trace.ActualWidth, "actual_height", trace.ActualHeight)
	}
	if trace.FrameSimilaritySummary != "" {
		attributes = append(attributes, "frame_similarity_summary", trace.FrameSimilaritySummary)
	}
	if trace.QualityRejectionReason != "" {
		attributes = append(attributes, "quality_rejection_reason", trace.QualityRejectionReason)
	}
	if trace.QualityEvidenceDir != "" {
		attributes = append(attributes, "quality_evidence_dir", trace.QualityEvidenceDir)
	}
	return attributes
}

func videoResultURLAttrs(raw string) []any {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return []any{"result_url_present", false}
	}
	host := ""
	hasQuery := false
	if parsed, err := url.Parse(raw); err == nil && parsed != nil {
		host = parsed.Hostname()
		hasQuery = parsed.RawQuery != ""
	}
	sum := sha256.Sum256([]byte(raw))
	return []any{
		"result_url_present", true,
		"result_url_host", host,
		"result_url_has_query", hasQuery,
		"result_url_fingerprint", hex.EncodeToString(sum[:8]),
	}
}

func formatVideoFrameSimilarity(probe videoQualityProbe, spec videoQualitySpec) string {
	summary := summarizeVideoQualityFrames(probe.Frames, spec.referenceHashes)
	if len(summary) == 0 {
		return "frames=0"
	}
	matched := 0
	for _, frame := range summary {
		if frame.MatchesReference {
			matched++
		}
	}
	return fmt.Sprintf("matched=%d/%d", matched, len(summary))
}

func saveVideoQualityEvidence(temporaryPath, jobID string, spec videoQualitySpec, probe videoQualityProbe, qualityErr *videoQualityError) (string, error) {
	root := strings.TrimSpace(os.Getenv("GROK2API_QUALITY_GUARD_DIR"))
	if root == "" {
		return "", nil
	}
	jobDirectory := filepath.Join(root, videoQualityEvidenceDirectory, safeVideoQualityEvidencePathComponent(jobID))
	if err := os.MkdirAll(jobDirectory, 0o700); err != nil {
		return "", fmt.Errorf("create video quality evidence directory: %w", err)
	}
	evidenceDirectory, err := os.MkdirTemp(jobDirectory, "attempt-")
	if err != nil {
		return "", fmt.Errorf("create video quality evidence attempt directory: %w", err)
	}
	keepEvidence := false
	defer func() {
		if !keepEvidence {
			_ = os.RemoveAll(evidenceDirectory)
		}
	}()

	if err := copyVideoQualityEvidence(filepath.Join(evidenceDirectory, videoQualityEvidenceVideoFile), temporaryPath); err != nil {
		return "", err
	}
	manifest := videoQualityEvidenceManifest{
		CollectedAt:          time.Now().UTC().Format(time.RFC3339Nano),
		JobID:                jobID,
		Reason:               qualityErr.Error(),
		RequestedAspectRatio: spec.AspectRatio,
		ActualWidth:          probe.Width,
		ActualHeight:         probe.Height,
		FirstFrameMode:       spec.FirstFrameMode,
		ReferenceMode:        spec.ReferenceMode,
		ReferenceImageCount:  len(spec.referenceHashes),
		FrameProbeError:      probe.FrameProbeError,
		Frames:               summarizeVideoQualityFrames(probe.Frames, spec.referenceHashes),
	}
	encodedManifest, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return "", fmt.Errorf("encode video quality evidence manifest: %w", err)
	}
	if err := os.WriteFile(filepath.Join(evidenceDirectory, videoQualityEvidenceManifestFile), encodedManifest, 0o600); err != nil {
		return "", fmt.Errorf("write video quality evidence manifest: %w", err)
	}
	keepEvidence = true
	return evidenceDirectory, nil
}

func copyVideoQualityEvidence(destinationPath, sourcePath string) error {
	source, err := os.Open(sourcePath)
	if err != nil {
		return fmt.Errorf("open rejected video for evidence: %w", err)
	}
	defer source.Close()
	destination, err := os.OpenFile(destinationPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create rejected video evidence: %w", err)
	}
	if _, err := io.Copy(destination, source); err != nil {
		_ = destination.Close()
		return fmt.Errorf("copy rejected video evidence: %w", err)
	}
	if err := destination.Close(); err != nil {
		return fmt.Errorf("close rejected video evidence: %w", err)
	}
	return nil
}

func safeVideoQualityEvidencePathComponent(value string) string {
	const fallback = "unknown-job"
	value = strings.TrimSpace(value)
	if value == "" {
		return fallback
	}
	var builder strings.Builder
	builder.Grow(min(len(value), 96))
	for _, character := range value {
		if (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') || (character >= '0' && character <= '9') || character == '-' || character == '_' {
			builder.WriteRune(character)
		} else {
			builder.WriteByte('_')
		}
		if builder.Len() >= 96 {
			break
		}
	}
	if result := strings.Trim(builder.String(), "._-"); result != "" {
		return result
	}
	return fallback
}

func summarizeVideoQualityFrames(frames, references []videoImageHash) []videoQualityFrameEvidence {
	if len(frames) == 0 {
		return nil
	}
	summary := make([]videoQualityFrameEvidence, 0, len(frames))
	for index, frame := range frames {
		frameEvidence := videoQualityFrameEvidence{Index: index, MinimumHammingDistance: -1, MinimumAverageColorDelta: -1}
		for _, reference := range references {
			hammingDistance := bits.OnesCount64(frame.luminance ^ reference.luminance)
			averageColorDelta := videoAverageColorDistance(frame, reference)
			if frameEvidence.MinimumHammingDistance < 0 || hammingDistance < frameEvidence.MinimumHammingDistance {
				frameEvidence.MinimumHammingDistance = hammingDistance
			}
			if frameEvidence.MinimumAverageColorDelta < 0 || averageColorDelta < frameEvidence.MinimumAverageColorDelta {
				frameEvidence.MinimumAverageColorDelta = averageColorDelta
			}
			if hammingDistance <= 14 && averageColorDelta <= 150 {
				frameEvidence.MatchesReference = true
			}
		}
		summary = append(summary, frameEvidence)
	}
	return summary
}

func (s *Service) inspectVideoQuality(ctx context.Context, path string) (videoQualityProbe, error) {
	if s.videoQualityInspector != nil {
		return s.videoQualityInspector.Inspect(ctx, path)
	}
	return ffmpegVideoQualityInspector{}.Inspect(ctx, path)
}

type ffmpegVideoQualityInspector struct{}

func (ffmpegVideoQualityInspector) Inspect(ctx context.Context, path string) (videoQualityProbe, error) {
	probe, duration, err := inspectVideoQualityWithFFprobe(ctx, path)
	if err != nil {
		probe, duration, err = inspectVideoQualityWithFFmpeg(ctx, path)
		if err != nil {
			return videoQualityProbe{}, fmt.Errorf("video quality metadata probe: %w", err)
		}
	}
	if duration <= 0 {
		return probe, nil
	}
	for _, fraction := range [...]float64{0.02, 0.5, 0.9} {
		frame, frameErr := extractVideoQualityFrame(ctx, path, duration*fraction)
		if frameErr != nil {
			probe.FrameProbeError = frameErr.Error()
			return probe, nil
		}
		probe.Frames = append(probe.Frames, frame)
	}
	return probe, nil
}

func inspectVideoQualityWithFFprobe(ctx context.Context, path string) (videoQualityProbe, float64, error) {
	dimensions, err := exec.CommandContext(ctx, "ffprobe", "-v", "error", "-select_streams", "v:0", "-show_entries", "stream=width,height", "-of", "csv=p=0", path).Output()
	if err != nil {
		return videoQualityProbe{}, 0, fmt.Errorf("ffprobe video dimensions: %w", err)
	}
	parts := strings.Split(strings.TrimSpace(string(dimensions)), ",")
	if len(parts) != 2 {
		return videoQualityProbe{}, 0, fmt.Errorf("ffprobe did not return video dimensions")
	}
	width, widthErr := strconv.Atoi(strings.TrimSpace(parts[0]))
	height, heightErr := strconv.Atoi(strings.TrimSpace(parts[1]))
	if widthErr != nil || heightErr != nil || width <= 0 || height <= 0 {
		return videoQualityProbe{}, 0, fmt.Errorf("ffprobe returned invalid video dimensions")
	}
	probe := videoQualityProbe{Width: width, Height: height}
	durationOutput, durationCommandErr := exec.CommandContext(ctx, "ffprobe", "-v", "error", "-show_entries", "format=duration", "-of", "default=noprint_wrappers=1:nokey=1", path).Output()
	if durationCommandErr != nil {
		return probe, 0, nil
	}
	duration, durationErr := strconv.ParseFloat(strings.TrimSpace(string(durationOutput)), 64)
	if durationErr != nil || duration <= 0 {
		return probe, 0, nil
	}
	return probe, duration, nil
}

func inspectVideoQualityWithFFmpeg(ctx context.Context, path string) (videoQualityProbe, float64, error) {
	output, commandErr := exec.CommandContext(ctx, "ffmpeg", "-hide_banner", "-i", path).CombinedOutput()
	probe, duration, parseErr := parseFFmpegVideoQualityMetadata(output)
	if parseErr != nil {
		if commandErr != nil {
			return videoQualityProbe{}, 0, fmt.Errorf("ffmpeg fallback probe: %w", commandErr)
		}
		return videoQualityProbe{}, 0, fmt.Errorf("parse ffmpeg video metadata: %w", parseErr)
	}
	return probe, duration, nil
}

var (
	ffmpegVideoQualityDurationPattern   = regexp.MustCompile(`(?m)^\s*Duration:\s*(\d{2}):(\d{2}):(\d{2}(?:\.\d+)?)`)
	ffmpegVideoQualityDimensionsPattern = regexp.MustCompile(`(?m)Video:.*?(\d{2,5})x(\d{2,5})(?:[,\s])`)
)

func parseFFmpegVideoQualityMetadata(output []byte) (videoQualityProbe, float64, error) {
	metadata := string(output)
	dimensions := ffmpegVideoQualityDimensionsPattern.FindStringSubmatch(metadata)
	if len(dimensions) != 3 {
		return videoQualityProbe{}, 0, fmt.Errorf("ffmpeg did not return video dimensions")
	}
	width, widthErr := strconv.Atoi(dimensions[1])
	height, heightErr := strconv.Atoi(dimensions[2])
	if widthErr != nil || heightErr != nil || width <= 0 || height <= 0 {
		return videoQualityProbe{}, 0, fmt.Errorf("ffmpeg returned invalid video dimensions")
	}
	probe := videoQualityProbe{Width: width, Height: height}
	durationParts := ffmpegVideoQualityDurationPattern.FindStringSubmatch(metadata)
	if len(durationParts) != 4 {
		return probe, 0, nil
	}
	hours, hourErr := strconv.ParseFloat(durationParts[1], 64)
	minutes, minuteErr := strconv.ParseFloat(durationParts[2], 64)
	seconds, secondErr := strconv.ParseFloat(durationParts[3], 64)
	if hourErr != nil || minuteErr != nil || secondErr != nil || hours < 0 || minutes < 0 || seconds <= 0 {
		return probe, 0, nil
	}
	return probe, hours*3600 + minutes*60 + seconds, nil
}

func extractVideoQualityFrame(ctx context.Context, path string, offsetSeconds float64) (videoImageHash, error) {
	offset := strconv.FormatFloat(max(0, offsetSeconds), 'f', 3, 64)
	output, err := exec.CommandContext(ctx, "ffmpeg", "-v", "error", "-ss", offset, "-i", path, "-frames:v", "1", "-vf", "scale=64:64:force_original_aspect_ratio=decrease", "-f", "image2pipe", "-vcodec", "png", "pipe:1").Output()
	if err != nil {
		return videoImageHash{}, fmt.Errorf("ffmpeg 抽帧: %w", err)
	}
	frame, _, err := image.Decode(bytes.NewReader(output))
	if err != nil {
		return videoImageHash{}, fmt.Errorf("解析 ffmpeg 抽帧: %w", err)
	}
	return hashVideoQualityImage(frame)
}

func hashVideoQualityImage(source image.Image) (videoImageHash, error) {
	if source == nil {
		return videoImageHash{}, fmt.Errorf("quality image is nil")
	}
	bounds := source.Bounds()
	width, height := bounds.Dx(), bounds.Dy()
	if width <= 0 || height <= 0 {
		return videoImageHash{}, fmt.Errorf("quality image has invalid bounds")
	}

	const side = 8
	var luminance [side * side]uint16
	var totalLuminance uint64
	var totalRed, totalGreen, totalBlue uint64
	for y := 0; y < side; y++ {
		for x := 0; x < side; x++ {
			sx := bounds.Min.X + min(width-1, ((2*x+1)*width)/(2*side))
			sy := bounds.Min.Y + min(height-1, ((2*y+1)*height)/(2*side))
			r, g, b, _ := source.At(sx, sy).RGBA()
			index := y*side + x
			// ITU-R BT.601 luminance. The 16-bit image values avoid a
			// lossy conversion before the average has been calculated.
			value := uint16((299*uint64(r) + 587*uint64(g) + 114*uint64(b)) / 1000)
			luminance[index] = value
			totalLuminance += uint64(value)
			totalRed += uint64(r)
			totalGreen += uint64(g)
			totalBlue += uint64(b)
		}
	}
	average := uint16(totalLuminance / (side * side))
	var value uint64
	for index, luminanceValue := range luminance {
		if luminanceValue >= average {
			value |= uint64(1) << index
		}
	}
	return videoImageHash{
		luminance: value,
		red:       uint8(totalRed / (side * side) >> 8),
		green:     uint8(totalGreen / (side * side) >> 8),
		blue:      uint8(totalBlue / (side * side) >> 8),
	}, nil
}

func evaluateVideoQuality(probe videoQualityProbe, spec videoQualitySpec, references []videoImageHash) *videoQualityError {
	if orientation := requestedVideoOrientation(spec.AspectRatio); orientation != 0 {
		if actual := videoOrientation(probe.Width, probe.Height); actual != 0 && actual != orientation {
			return &videoQualityError{reason: "成片方向与请求比例不一致"}
		}
	}

	// 首帧模式要求第 0 帧继承输入图。抽帧缺失时不据此拒片。
	if spec.FirstFrameMode && len(references) > 0 && len(probe.Frames) > 0 &&
		!videoFrameMatchesAnyReference(probe.Frames[0], references[:1]) {
		return &videoQualityError{reason: "首帧未继承输入图片"}
	}
	// 参考图模式仍要求抽帧里至少一帧相似。探测不完整时放行。
	if !spec.ReferenceMode || len(references) == 0 || len(probe.Frames) < 3 {
		return nil
	}
	matched := 0
	for _, frame := range probe.Frames {
		if videoFrameMatchesAnyReference(frame, references) {
			matched++
		}
	}
	if matched == 0 {
		return &videoQualityError{reason: "参考图主体未出现在抽帧中"}
	}
	return nil
}

func requestedVideoOrientation(aspectRatio string) int {
	parts := strings.Split(strings.TrimSpace(aspectRatio), ":")
	if len(parts) != 2 {
		return 0
	}
	width, widthErr := parseVideoQualityRatioPart(parts[0])
	height, heightErr := parseVideoQualityRatioPart(parts[1])
	if widthErr != nil || heightErr != nil || width <= 0 || height <= 0 {
		return 0
	}
	ratio := width / height
	if math.Abs(ratio-1) < 0.05 {
		return 0
	}
	if ratio > 1 {
		return 1
	}
	return -1
}

func parseVideoQualityRatioPart(value string) (float64, error) {
	return strconv.ParseFloat(strings.TrimSpace(value), 64)
}

func videoOrientation(width, height int) int {
	if width <= 0 || height <= 0 || width == height {
		return 0
	}
	if width > height {
		return 1
	}
	return -1
}

func videoFrameMatchesAnyReference(frame videoImageHash, references []videoImageHash) bool {
	for _, reference := range references {
		if bits.OnesCount64(frame.luminance^reference.luminance) <= 14 && videoAverageColorDistance(frame, reference) <= 150 {
			return true
		}
	}
	return false
}

func videoAverageColorDistance(first, second videoImageHash) int {
	return absVideoColor(int(first.red)-int(second.red)) +
		absVideoColor(int(first.green)-int(second.green)) +
		absVideoColor(int(first.blue)-int(second.blue))
}

func absVideoColor(value int) int {
	if value < 0 {
		return -value
	}
	return value
}
