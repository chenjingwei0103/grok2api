package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/url"
	"strings"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
)

// videoQualityAccountSwitches is the number of replacement accounts allowed
// after the first completed upstream video is rejected. The initial account is
// not counted, so a job can use at most seven accounts for this failure mode.
const videoQualityAccountSwitches = 6

type videoQualityError struct {
	reason string
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

// evaluateUpstreamVideoQuality deliberately relies only on the two upstream
// moderation fields. A Web stream with no usable moderated flag is the known
// fallback-template shape and is rejected before its video is downloaded.
// Other providers do not expose that Web stream, so they are not judged here.
func evaluateUpstreamVideoQuality(metadata provider.VideoUpstreamMetadata) *videoQualityError {
	if !metadata.StreamObserved {
		return nil
	}
	if !metadata.ModeratedPresent {
		return &videoQualityError{reason: "上游视频响应缺少 moderated 标记，疑似官方兜底模板"}
	}
	if metadata.Moderated {
		return &videoQualityError{reason: "上游将视频标记为 moderated"}
	}
	return nil
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
	ModeratedPresent       bool
	Moderated              bool
	QualityRejectionReason string
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

func (s *Service) bindVideoDownloadTrace(ctx context.Context, jobID string, credential account.Credential, result provider.VideoResult) (context.Context, *videoUpstreamTrace) {
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
	trace.ResultURL = result.URL
	if result.AssetID != "" {
		trace.ResultAssetID = result.AssetID
	}
	return ctx, trace
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
		"moderated_present", trace.ModeratedPresent,
		"moderated", trace.Moderated,
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
	if trace.QualityRejectionReason != "" {
		attributes = append(attributes, "quality_rejection_reason", trace.QualityRejectionReason)
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
