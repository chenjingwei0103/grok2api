package web

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	domainegress "github.com/chenyme/grok2api/backend/internal/domain/egress"
	infraegress "github.com/chenyme/grok2api/backend/internal/infra/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
)

type webMediaUpstreamError struct {
	status              int
	summary             string
	bodyBytes           int
	bodyTruncated       bool
	bodyPrefixSHA256    string
	bodyKind            string
	cloudflareChallenge bool
}

func (e *webMediaUpstreamError) Error() string {
	if e == nil {
		return ""
	}
	return e.summary
}

func (e *webMediaUpstreamError) HTTPStatusCode() int {
	if e == nil {
		return 0
	}
	return e.status
}

// isClearanceRefreshableMediaError distinguishes browser-session challenges
// from structured upstream policy responses such as content moderation. Empty
// and HTML 403 bodies are the forms returned by the media endpoints when the
// request is rejected before the application response is built.
func isClearanceRefreshableMediaError(e *webMediaUpstreamError) bool {
	if e == nil || e.status != http.StatusForbidden {
		return false
	}
	return e.cloudflareChallenge || e.bodyKind == "empty" || e.bodyKind == "html"
}

// isStatsigRefreshableMediaError identifies application-layer rejections that
// tell the browser to reload its page state. Grok uses code 7 for this anti-bot
// response, but the same code can also wrap a definitive account block; blocked
// credentials must remain terminal and must not be replayed with a fresh signature.
func isStatsigRefreshableMediaError(e *webMediaUpstreamError, body []byte) bool {
	if e == nil || e.status != http.StatusForbidden || e.bodyKind != "json" || provider.IsDefinitiveAccountBlockBody(body) {
		return false
	}
	code, message, structured := extractWebMediaUpstreamErrorFields(body)
	if !structured {
		return false
	}
	normalized := strings.ToLower(message)
	return code == "7" || strings.Contains(normalized, "anti-bot") ||
		strings.Contains(normalized, "page is out of date") || strings.Contains(normalized, "reload to continue")
}

func (e *webMediaUpstreamError) providerResponse() *provider.Response {
	if e == nil {
		return nil
	}
	code := "upstream_forbidden"
	if e.status != http.StatusForbidden {
		code = "upstream_unavailable"
	}
	return jsonProviderResponse(e.status, map[string]any{"error": map[string]any{
		"message": e.summary,
		"type":    "upstream_error",
		"code":    code,
	}})
}

const (
	webMediaDiagnosticBodyLimit    = 64 << 10
	webMediaDiagnosticSummaryLimit = 256
	webMediaDiagnosticFieldLimit   = 160
)

var (
	webMediaAuthorizationPattern = regexp.MustCompile(`(?i)\b(bearer|basic)\s+[A-Za-z0-9._~+/=-]+`)
	webMediaCookiePattern        = regexp.MustCompile(`(?i)\b(cookie|set-cookie)\b\s*[:=]\s*[^\r\n]+`)
	webMediaSecretPattern        = regexp.MustCompile(`(?i)(["']?(?:authorization|proxy-authorization|x-api-key|api[_-]?key|access[_-]?token|refresh[_-]?token|id[_-]?token|upload[_-]?url|cookie|sso|session[_-]?id)["']?\s*[:=]\s*["']?)[^"'\s,;}]+`)
	webMediaJWTPattern           = regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{12,}\.[A-Za-z0-9_-]{12,}(?:\.[A-Za-z0-9_-]{12,})?\b`)
	webMediaEmailPattern         = regexp.MustCompile(`(?i)\b[A-Z0-9._%+-]+@[A-Z0-9.-]+\.[A-Z]{2,}\b`)
	webMediaURLPattern           = regexp.MustCompile(`https?://[^\s"'<>]+`)
	webMediaLongTokenPattern     = regexp.MustCompile(`[A-Za-z0-9+/=_-]{256,}`)
)

// newWebMediaUpstreamError keeps the HTTP status while exposing only a
// bounded, redacted summary through the error. Structured logs retain only
// body metadata and a prefix hash, never the upstream response body itself.
func newWebMediaUpstreamError(status int, body []byte, truncated bool) *webMediaUpstreamError {
	digest := sha256.Sum256(body)
	return &webMediaUpstreamError{
		status:              status,
		summary:             summarizeWebMediaUpstreamError(status, body, truncated),
		bodyBytes:           len(body),
		bodyTruncated:       truncated,
		bodyPrefixSHA256:    fmt.Sprintf("%x", digest),
		bodyKind:            classifyWebMediaDiagnosticBody(body),
		cloudflareChallenge: isCloudflareChallengeBody(body),
	}
}

func classifyWebMediaDiagnosticBody(body []byte) string {
	if !utf8.Valid(body) {
		return "binary"
	}
	trimmed := strings.TrimSpace(string(body))
	if trimmed == "" {
		return "empty"
	}
	if json.Valid(body) {
		return "json"
	}
	lower := strings.ToLower(trimmed)
	if strings.HasPrefix(lower, "<!doctype html") || strings.HasPrefix(lower, "<html") {
		return "html"
	}
	for _, value := range trimmed {
		if value < 0x20 && value != '\t' && value != '\r' && value != '\n' {
			return "binary"
		}
	}
	return "text"
}

func isCloudflareChallengeBody(body []byte) bool {
	lower := strings.ToLower(string(body))
	return strings.Contains(lower, "just a moment") ||
		strings.Contains(lower, "challenge-platform") ||
		strings.Contains(lower, "__cf_chl") ||
		strings.Contains(lower, "cf-chl-")
}

func (a *Adapter) logWebMediaUpstreamRejection(stage string, response *http.Response, upstreamErr *webMediaUpstreamError) {
	if upstreamErr == nil {
		return
	}
	attributes := []any{
		"stage", stage,
		"status", upstreamErr.status,
		"body_bytes_captured", upstreamErr.bodyBytes,
		"body_truncated", upstreamErr.bodyTruncated,
		"body_prefix_sha256", upstreamErr.bodyPrefixSHA256,
		"body_kind", upstreamErr.bodyKind,
		"cloudflare_challenge", upstreamErr.cloudflareChallenge,
	}
	if response != nil {
		attributes = append(attributes,
			"content_type", safeWebMediaDiagnostic(response.Header.Get("Content-Type"), 128),
			"content_length", response.ContentLength,
			"content_encoding", safeWebMediaDiagnostic(response.Header.Get("Content-Encoding"), 64),
			"server", safeWebMediaDiagnostic(response.Header.Get("Server"), 128),
			"cf_ray", safeWebMediaDiagnostic(response.Header.Get("CF-Ray"), 128),
			"upstream_request_id", safeWebMediaDiagnostic(firstNonEmpty(response.Header.Get("X-Request-Id"), response.Header.Get("X-Xai-Request-Id")), 128),
		)
	}
	a.log().Warn("web_media_upstream_rejected", attributes...)
}

func (a *Adapter) logVideoTrace(event string, attributes map[string]any) {
	if len(attributes) == 0 {
		a.log().Info(event)
		return
	}
	keys := make([]string, 0, len(attributes))
	for key := range attributes {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	args := make([]any, 0, len(keys)*2)
	for _, key := range keys {
		args = append(args, key, attributes[key])
	}
	a.log().Info(event, args...)
}

func sha256HexForVideoTrace(value string) string {
	return sha256HexForVideoTraceBytes([]byte(value))
}

func sha256HexForVideoTraceBytes(value []byte) string {
	digest := sha256.Sum256(value)
	return fmt.Sprintf("%x", digest)
}

func videoAssetIDHashes(values []string) []string {
	hashes := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		hashes = append(hashes, sha256HexForVideoTrace(value))
	}
	return hashes
}

func videoRequestedReferenceCount(values []string) int {
	count := 0
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			count++
		}
	}
	return count
}

func videoTraceAttributes(request provider.VideoRequest, firstFrameAsset string, referenceAssets []string) map[string]any {
	firstFrameAsset = strings.TrimSpace(firstFrameAsset)
	assets := make([]string, 0, len(referenceAssets)+1)
	if firstFrameAsset != "" {
		assets = append(assets, firstFrameAsset)
	}
	for _, asset := range referenceAssets {
		if strings.TrimSpace(asset) != "" {
			assets = append(assets, strings.TrimSpace(asset))
		}
	}

	referenceCount := 0
	for _, asset := range referenceAssets {
		if strings.TrimSpace(asset) != "" {
			referenceCount++
		}
	}
	if referenceCount == 0 {
		referenceCount = videoRequestedReferenceCount(request.ReferenceURLs)
	}
	mode := "textToVideo"
	switch {
	case referenceCount >= 2:
		mode = "referenceToVideo"
	case referenceCount == 1 || strings.TrimSpace(request.ImageURL) != "" || firstFrameAsset != "":
		mode = "imageToVideo"
	}
	attributes := map[string]any{
		"job_id":                    safeWebMediaDiagnostic(request.JobID, 128),
		"model":                     safeWebMediaDiagnostic(request.Model, 64),
		"mode":                      mode,
		"requested_first_frame":     strings.TrimSpace(request.ImageURL) != "",
		"requested_reference_count": videoRequestedReferenceCount(request.ReferenceURLs),
		"first_frame_asset_count":   boolToInt(firstFrameAsset != ""),
		"reference_asset_count":     len(referenceAssets),
		"input_asset_count":         len(assets),
		"input_asset_sha256":        videoAssetIDHashes(assets),
		"prompt_bytes":              len([]byte(request.Prompt)),
		"prompt_sha256":             sha256HexForVideoTrace(request.Prompt),
		"duration_seconds":          request.Duration,
		"aspect_ratio":              safeWebMediaDiagnostic(request.AspectRatio, 32),
		"resolution":                safeWebMediaDiagnostic(request.Resolution, 32),
	}
	return attributes
}

func boolToInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func videoSourceKind(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "empty"
	}
	if strings.HasPrefix(strings.ToLower(value), "data:") {
		return "data_url"
	}
	return "remote_url"
}

func missingVideoAssets(echoed, required []string) []string {
	missing := make([]string, 0)
	for _, assetID := range required {
		if assetID != "" && !containsString(echoed, assetID) {
			missing = append(missing, assetID)
		}
	}
	return missing
}

func summarizeWebMediaUpstreamError(status int, body []byte, truncated bool) string {
	code, message, structured := extractWebMediaUpstreamErrorFields(body)
	parts := []string{fmt.Sprintf("Grok Web 媒体上游返回 %d", status)}
	if code != "" {
		parts = append(parts, code)
	}
	if message != "" {
		parts = append(parts, message)
	} else if len(strings.TrimSpace(string(body))) == 0 {
		parts = append(parts, "<empty>")
	} else if truncated {
		parts = append(parts, "响应正文过长")
	} else if !structured {
		parts = append(parts, "响应正文不可解析")
	} else if code == "" {
		parts = append(parts, "未提供错误详情")
	}
	return boundWebMediaDiagnostic(strings.Join(parts, ": "), webMediaDiagnosticSummaryLimit)
}

func extractWebMediaUpstreamErrorFields(body []byte) (code, message string, structured bool) {
	var root map[string]any
	if err := json.Unmarshal(body, &root); err != nil {
		return "", "", false
	}
	structured = true
	if errorObject, ok := root["error"].(map[string]any); ok {
		code = firstWebMediaDiagnosticCode(errorObject, "code", "type", "error")
		message = firstString(errorObject, "message", "error", "detail")
	} else if errorText, ok := root["error"].(string); ok {
		message = errorText
	}
	if code == "" {
		code = firstWebMediaDiagnosticCode(root, "code", "error_code", "type")
	}
	if message == "" {
		message = firstString(root, "message", "error_message", "detail")
	}
	return safeWebMediaDiagnostic(code, 64), safeWebMediaDiagnostic(message, webMediaDiagnosticFieldLimit), true
}

func firstWebMediaDiagnosticCode(value map[string]any, keys ...string) string {
	if code := firstString(value, keys...); code != "" {
		return code
	}
	if code, ok := firstInt(value, keys...); ok {
		return fmt.Sprintf("%d", code)
	}
	return ""
}

func safeWebMediaDiagnostic(value string, limit int) string {
	value = strings.Join(strings.Fields(value), " ")
	value = webMediaCookiePattern.ReplaceAllString(value, "$1: [REDACTED]")
	value = webMediaAuthorizationPattern.ReplaceAllString(value, "$1 [REDACTED]")
	value = webMediaSecretPattern.ReplaceAllString(value, "$1[REDACTED]")
	value = webMediaJWTPattern.ReplaceAllString(value, "[REDACTED]")
	value = webMediaEmailPattern.ReplaceAllString(value, "[REDACTED_EMAIL]")
	value = webMediaURLPattern.ReplaceAllString(value, "[REDACTED_URL]")
	value = webMediaLongTokenPattern.ReplaceAllString(value, "[REDACTED_LONG_VALUE]")
	return boundWebMediaDiagnostic(value, limit)
}

func boundWebMediaDiagnostic(value string, limit int) string {
	if limit <= 0 || value == "" {
		return ""
	}
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit])
}

func (a *Adapter) GenerateVideo(ctx context.Context, request provider.VideoRequest) (provider.VideoResult, error) {
	a.logVideoTrace("video_generation_started", videoTraceAttributes(request, "", nil))
	if len(request.ReferenceAudios) > 0 {
		return provider.VideoResult{}, provider.WrapVideoStage(provider.VideoStagePrepare, 0, fmt.Errorf("Grok Web 当前不支持 reference_audios"))
	}
	cfg := a.config()
	token, err := a.cipher.Decrypt(request.Credential.EncryptedAccessToken)
	if err != nil {
		return provider.VideoResult{}, provider.WrapVideoStage(provider.VideoStagePrepare, 0, err)
	}
	lease, err := a.egress.AcquireCredential(ctx, domainegress.ScopeWeb, request.Credential)
	if err != nil {
		return provider.VideoResult{}, provider.WrapVideoStage(provider.VideoStagePrepare, 0, err)
	}
	defer lease.Release()
	segments := videoSegments(request.Duration)
	if len(segments) == 0 {
		return provider.VideoResult{}, provider.WrapVideoStage(provider.VideoStagePrepare, 0, fmt.Errorf("duration 必须在 1 到 15 秒之间"))
	}
	ratio := resolveAspectRatio(request.AspectRatio)
	resolution := request.Resolution
	if resolution == "" {
		resolution = "720p"
	}
	firstFrameAsset, referenceAssets, err := a.uploadVideoReferenceAssets(ctx, cfg, lease, token, request)
	if err != nil {
		attributes := videoTraceAttributes(request, firstFrameAsset, referenceAssets)
		attributes["phase"] = "reference_upload"
		attributes["error"] = safeWebMediaDiagnostic(err.Error(), webMediaDiagnosticSummaryLimit)
		a.logVideoTrace("video_reference_upload_failed", attributes)
		return provider.VideoResult{}, provider.WrapVideoStage(provider.VideoStagePrepare, 0, err)
	}
	attributes := videoTraceAttributes(request, firstFrameAsset, referenceAssets)
	attributes["phase"] = "reference_upload_complete"
	a.logVideoTrace("video_reference_uploads_completed", attributes)
	if err := a.prepareVideoImagineSession(ctx, cfg, lease, token, firstFrameAsset, referenceAssets); err != nil {
		attributes := videoTraceAttributes(request, firstFrameAsset, referenceAssets)
		attributes["phase"] = "asset_visibility_check"
		attributes["error"] = safeWebMediaDiagnostic(err.Error(), webMediaDiagnosticSummaryLimit)
		a.logVideoTrace("video_reference_binding_failed", attributes)
		return provider.VideoResult{}, provider.WrapVideoStage(provider.VideoStagePrepare, 0, err)
	}
	attributes = videoTraceAttributes(request, firstFrameAsset, referenceAssets)
	attributes["phase"] = "asset_visibility_check_passed"
	a.logVideoTrace("video_reference_assets_visible", attributes)
	payload := videoCreatePayload(request.Prompt, ratio, resolution, segments[0], firstFrameAsset, referenceAssets)
	attributes = videoTraceAttributes(request, firstFrameAsset, referenceAssets)
	attributes["phase"] = "create_payload"
	a.logVideoTrace("video_create_payload", attributes)
	response, err := a.postJSONWithReferer(ctx, cfg, lease, token, cfg.BaseURL+"/rest/app-chat/conversations/new", payload, time.Duration(cfg.VideoTimeoutSeconds)*time.Second, cfg.BaseURL+"/imagine")
	if err != nil {
		attributes := videoTraceAttributes(request, firstFrameAsset, referenceAssets)
		attributes["phase"] = "create_request"
		attributes["error"] = safeWebMediaDiagnostic(err.Error(), webMediaDiagnosticSummaryLimit)
		a.logVideoTrace("video_create_request_failed", attributes)
		return provider.VideoResult{}, provider.WrapVideoStage(provider.VideoCreateFailureStage(err), 0, err)
	}
	result, _, echoedAssets, parseErr := parseVideoStream(response, request.Progress)
	_ = response.Body.Close()
	attributes = videoTraceAttributes(request, firstFrameAsset, referenceAssets)
	attributes["phase"] = "upstream_response"
	attributes["upstream_status"] = response.StatusCode
	attributes["upstream_echoed_asset_count"] = len(echoedAssets)
	attributes["upstream_echoed_asset_sha256"] = videoAssetIDHashes(echoedAssets)
	attributes["video_url_present"] = result.URL != ""
	if parseErr != nil {
		attributes["error"] = safeWebMediaDiagnostic(parseErr.Error(), webMediaDiagnosticSummaryLimit)
	}
	a.logVideoTrace("video_upstream_response", attributes)
	if parseErr != nil {
		if upstreamErr, ok := parseErr.(*webMediaUpstreamError); ok {
			a.logWebMediaUpstreamRejection("video_generation", response, upstreamErr)
		}
		stage := provider.VideoStagePoll
		if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
			stage = provider.VideoCreateFailureStage(parseErr)
		}
		return provider.VideoResult{}, provider.WrapVideoStage(stage, 0, parseErr)
	}
	if result.URL == "" {
		return provider.VideoResult{}, provider.WrapVideoStage(provider.VideoStagePoll, 0, fmt.Errorf("视频生成完成但没有返回内容 URL"))
	}
	requiredAssets := make([]string, 0, len(referenceAssets)+1)
	if firstFrameAsset != "" {
		requiredAssets = append(requiredAssets, firstFrameAsset)
	}
	requiredAssets = append(requiredAssets, referenceAssets...)
	missingAssets := missingVideoAssets(echoedAssets, requiredAssets)
	if len(missingAssets) > 0 {
		// A 200 that never echoes the uploaded asset did not bind the image.
		// Treat it as a create failure so the gateway can switch accounts
		// instead of saving an unrelated video.
		attributes := videoTraceAttributes(request, firstFrameAsset, referenceAssets)
		attributes["phase"] = "asset_binding"
		attributes["missing_asset_count"] = len(missingAssets)
		attributes["missing_asset_sha256"] = videoAssetIDHashes(missingAssets)
		a.logVideoTrace("video_reference_binding_failed", attributes)
		return provider.VideoResult{}, provider.WrapVideoStage(provider.VideoStageCreate, 0, fmt.Errorf("视频上游没有确认已接收参考图"))
	}
	attributes = videoTraceAttributes(request, firstFrameAsset, referenceAssets)
	attributes["phase"] = "completed"
	a.logVideoTrace("video_generation_completed", attributes)
	return result, nil
}

// prepareVideoImagineSession mirrors the browser's asset visibility checks after
// an Imagine upload and before creating an image-conditioned video request.
func (a *Adapter) prepareVideoImagineSession(ctx context.Context, cfg Config, lease *infraegress.Lease, token, firstFrameAsset string, referenceAssets []string) error {
	assets := make([]string, 0, len(referenceAssets)+1)
	if firstFrameAsset != "" {
		assets = append(assets, firstFrameAsset)
	}
	assets = append(assets, referenceAssets...)
	if len(assets) == 0 {
		return nil
	}
	response, err := a.postJSONWithReferer(ctx, cfg, lease, token, cfg.BaseURL+"/rest/media/collection/list", map[string]any{"limit": 100}, time.Duration(cfg.VideoTimeoutSeconds)*time.Second, cfg.BaseURL+"/imagine")
	if err != nil {
		return fmt.Errorf("初始化 Imagine 媒体集合: %w", err)
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		upstreamErr := readWebMediaUpstreamError(response)
		_ = response.Body.Close()
		return upstreamErr
	}
	_ = response.Body.Close()
	for _, assetID := range assets {
		if err := a.confirmVideoImagineAsset(ctx, cfg, lease, token, assetID); err != nil {
			return err
		}
	}
	return nil
}

func (a *Adapter) confirmVideoImagineAsset(ctx context.Context, cfg Config, lease *infraegress.Lease, token, assetID string) error {
	requestCtx, cancel := context.WithTimeout(ctx, time.Duration(cfg.VideoTimeoutSeconds)*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(requestCtx, http.MethodGet, cfg.BaseURL+"/rest/assets/"+url.PathEscape(assetID), nil)
	if err != nil {
		return err
	}
	request.Header = buildHeaders(token, lease, "")
	request.Header.Del("Content-Type")
	applyAppHeaders(request.Header, cfg.BaseURL, cfg.BaseURL+"/imagine")
	a.applySignedStatsig(requestCtx, request, token, lease)
	response, err := lease.DoDeferredForbidden(request)
	if err != nil {
		return err
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		upstreamErr := readWebMediaUpstreamError(response)
		_ = response.Body.Close()
		return upstreamErr
	}
	_ = response.Body.Close()
	return nil
}

func readWebMediaUpstreamError(response *http.Response) error {
	if response == nil {
		return errors.New("Grok Web 返回空响应")
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, webMediaDiagnosticBodyLimit+1))
	if err != nil {
		return fmt.Errorf("读取 Grok Web 错误响应: %w", err)
	}
	truncated := len(body) > webMediaDiagnosticBodyLimit
	if truncated {
		body = body[:webMediaDiagnosticBodyLimit]
	}
	return newWebMediaUpstreamError(response.StatusCode, body, truncated)
}
func (a *Adapter) uploadVideoReferenceAssets(ctx context.Context, cfg Config, lease *infraegress.Lease, token string, request provider.VideoRequest) (string, []string, error) {
	upload := func(rawURL, stage string, index int) (string, error) {
		a.logVideoTrace("video_reference_upload_started", map[string]any{
			"job_id":      safeWebMediaDiagnostic(request.JobID, 128),
			"stage":       stage,
			"input_index": index,
			"source_kind": videoSourceKind(rawURL),
		})
		image, err := a.loadChatImage(ctx, lease, rawURL, cfg.MaxInputImageBytes)
		if err != nil {
			return "", err
		}
		a.logVideoTrace("video_reference_image_loaded", map[string]any{
			"job_id":       safeWebMediaDiagnostic(request.JobID, 128),
			"stage":        stage,
			"input_index":  index,
			"mime_type":    safeWebMediaDiagnostic(image.MIMEType, 128),
			"image_bytes":  len(image.Data),
			"image_sha256": sha256HexForVideoTraceBytes(image.Data),
		})
		uploaded, err := a.uploadFileV2Direct(ctx, cfg, lease, token, image, cfg.BaseURL+"/imagine", imagineSelfUploadSource, stage)
		if err != nil {
			return "", err
		}
		if uploaded.MetadataID == "" {
			return "", fmt.Errorf("上传视频参考图成功但上游未返回 fileMetadataId")
		}
		a.logVideoTrace("video_reference_uploaded", map[string]any{
			"job_id":            safeWebMediaDiagnostic(request.JobID, 128),
			"stage":             stage,
			"input_index":       index,
			"asset_id_sha256":   sha256HexForVideoTrace(uploaded.MetadataID),
			"upload_id_present": uploaded.ID != "",
			"asset_uri_present": uploaded.URI != "",
		})
		return uploaded.MetadataID, nil
	}

	firstFrameAsset := ""
	if rawURL := strings.TrimSpace(request.ImageURL); rawURL != "" {
		asset, err := upload(rawURL, "video_first_frame_upload", 0)
		if err != nil {
			return "", nil, err
		}
		firstFrameAsset = asset
	}
	referenceAssets := make([]string, 0, len(request.ReferenceURLs))
	for index, rawURL := range request.ReferenceURLs {
		rawURL = strings.TrimSpace(rawURL)
		if rawURL == "" {
			continue
		}
		asset, err := upload(rawURL, "video_reference_upload", index)
		if err != nil {
			return "", nil, err
		}
		referenceAssets = append(referenceAssets, asset)
	}
	return firstFrameAsset, referenceAssets, nil
}

// DownloadVideo retrieves a completed Grok asset through its source SSO
// session. Direct asset URLs are not public and must not be exposed as a
// substitute for this authenticated transfer.
func (a *Adapter) DownloadVideo(ctx context.Context, credential account.Credential, rawURL string) (io.ReadCloser, string, int64, error) {
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || parsed.Scheme != "https" || !trustedImageAssetHost(parsed.Hostname()) || parsed.User != nil {
		return nil, "", 0, fmt.Errorf("视频内容 URL 不受信任")
	}
	token, err := a.cipher.Decrypt(credential.EncryptedAccessToken)
	if err != nil {
		return nil, "", 0, err
	}
	// 视频生成与成品下载必须复用同一账号身份；否则 Resin 会为 WebAsset
	// 重新分配租约，账号级 Cloudflare clearance 也不会进入下载请求。
	lease, err := a.egress.AcquireCredential(ctx, domainegress.ScopeWebAsset, credential)
	if err != nil {
		return nil, "", 0, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, parsed.String(), nil)
	if err != nil {
		lease.Release()
		return nil, "", 0, err
	}
	request.Header = buildHeaders(token, lease, "")
	request.Header.Del("Content-Type")
	response, err := lease.Do(request)
	if err != nil {
		a.egress.FeedbackForScope(context.WithoutCancel(ctx), domainegress.ScopeWebAsset, lease.NodeID, 0, err)
		lease.Release()
		return nil, "", 0, err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		_ = response.Body.Close()
		a.egress.FeedbackForScope(context.WithoutCancel(ctx), domainegress.ScopeWebAsset, lease.NodeID, response.StatusCode, nil)
		lease.Release()
		return nil, "", 0, fmt.Errorf("下载视频返回 %d", response.StatusCode)
	}
	contentType := strings.ToLower(strings.TrimSpace(strings.Split(response.Header.Get("Content-Type"), ";")[0]))
	if contentType == "" || contentType == "application/octet-stream" {
		contentType = "video/mp4"
	}
	if !strings.HasPrefix(contentType, "video/") {
		_ = response.Body.Close()
		lease.Release()
		return nil, "", 0, fmt.Errorf("上游视频 Content-Type 无效")
	}
	onFinished := func(readErr error, complete bool) {
		if readErr != nil {
			a.egress.FeedbackForScope(context.WithoutCancel(ctx), domainegress.ScopeWebAsset, lease.NodeID, 0, readErr)
		} else if complete {
			a.egress.FeedbackForScope(context.WithoutCancel(ctx), domainegress.ScopeWebAsset, lease.NodeID, response.StatusCode, nil)
		}
		lease.Release()
	}
	return provider.NewCompletionReadCloser(response.Body, onFinished), contentType, response.ContentLength, nil
}

func parseVideoStream(response *http.Response, progress func(int)) (provider.VideoResult, string, []string, error) {
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(response.Body, webMediaDiagnosticBodyLimit+1))
		if response.StatusCode == http.StatusUnauthorized {
			return provider.VideoResult{}, "", nil, provider.ErrUnauthorized
		}
		truncated := len(body) > webMediaDiagnosticBodyLimit
		if truncated {
			body = body[:webMediaDiagnosticBodyLimit]
		}
		return provider.VideoResult{}, "", nil, newWebMediaUpstreamError(response.StatusCode, body, truncated)
	}
	var result provider.VideoResult
	var postID string
	var echoedAssets []string
	handle := func(root map[string]any) (bool, error) {
		echoedAssets = collectVideoEchoAssets(echoedAssets, root)
		if errorValue, ok := root["error"].(map[string]any); ok {
			return false, webMediaStreamError(errorValue)
		}
		if errorValue := nestedMap(root, "result", "response", "error"); errorValue != nil {
			return false, webMediaStreamError(errorValue)
		}
		stream := nestedMap(root, "result", "response", "streamingVideoGenerationResponse")
		if stream != nil {
			if value, ok := numberAsInt(stream["progress"]); ok && progress != nil {
				progress(value)
			}
			if value, _ := stream["videoPostId"].(string); value != "" {
				postID = value
			} else if value, _ := stream["videoId"].(string); value != "" {
				postID = value
			}
			moderated, _ := stream["moderated"].(bool)
			if moderated {
				return false, nil
			}
			if setVideoResultURL(&result, firstString(stream, "videoUrl", "contentUrl", "contentURL", "assetUrl", "assetURL", "fileUri", "fileURL")) {
				return true, nil
			}
		}
		for _, attachment := range videoFileAttachments(root) {
			if setVideoResultURL(&result, attachment) {
				return true, nil
			}
		}
		return false, nil
	}

	reader := bufio.NewReader(response.Body)
	prefix, _ := reader.Peek(64)
	trimmedPrefix := strings.TrimSpace(string(prefix))
	var err error
	if strings.HasPrefix(trimmedPrefix, "data:") || strings.HasPrefix(trimmedPrefix, "event:") {
		err = consumeVideoSSE(reader, handle)
	} else {
		err = consumeVideoJSON(reader, handle)
	}
	if err != nil {
		return provider.VideoResult{}, "", nil, err
	}
	return result, postID, echoedAssets, nil
}

func collectVideoEchoAssets(dst []string, root map[string]any) []string {
	var walk func(any)
	walk = func(value any) {
		switch typed := value.(type) {
		case map[string]any:
			for key, child := range typed {
				if videoEchoAssetKey(key) {
					dst = appendVideoEchoAssetValues(dst, child)
				}
				walk(child)
			}
		case []any:
			for _, child := range typed {
				walk(child)
			}
		}
	}
	walk(root)
	return dst
}

func videoEchoAssetKey(key string) bool {
	switch key {
	case "fileAttachments", "inputAssets", "imageReferences", "resolvedImageReferences":
		return true
	default:
		return false
	}
}

func appendVideoEchoAssetValues(dst []string, value any) []string {
	switch typed := value.(type) {
	case string:
		return appendVideoEchoAssetID(dst, typed)
	case []any:
		for _, item := range typed {
			text, ok := item.(string)
			if !ok {
				continue
			}
			dst = appendVideoEchoAssetID(dst, text)
		}
	}
	return dst
}

func appendVideoEchoAssetID(dst []string, value string) []string {
	value = strings.TrimSpace(value)
	if value == "" {
		return dst
	}
	if strings.Contains(value, "://") || strings.Contains(value, "/") {
		path := value
		if parsed, err := url.Parse(value); err == nil && parsed.Path != "" {
			path = parsed.Path
		}
		for _, segment := range strings.Split(path, "/") {
			dst = appendVideoEchoAssetID(dst, segment)
		}
		return dst
	}
	if !videoEchoAssetSegment(value) || containsString(dst, value) {
		return dst
	}
	return append(dst, value)
}

func videoEchoAssetSegment(value string) bool {
	if len(value) < 8 || len(value) > 80 || strings.Contains(value, ".") {
		return false
	}
	switch value {
	case "content", "generated", "users", "videos":
		return false
	default:
		return true
	}
}

func videoStreamEchoesAssets(echoed, required []string) bool {
	if len(required) == 0 {
		return true
	}
	for _, assetID := range required {
		if assetID == "" {
			continue
		}
		if !containsString(echoed, assetID) {
			return false
		}
	}
	return true
}

func webMediaStreamError(value map[string]any) error {
	message := safeWebMediaDiagnostic(firstString(value, "message", "error", "detail"), webMediaDiagnosticFieldLimit)
	if message == "" {
		message = "未提供错误详情"
	}
	return fmt.Errorf("视频上游错误: %s", message)
}

func videoFileAttachments(root map[string]any) []string {
	modelResponse := nestedMap(root, "result", "response", "modelResponse")
	if modelResponse == nil {
		return nil
	}
	values, _ := modelResponse["fileAttachments"].([]any)
	attachments := make([]string, 0, len(values))
	for _, value := range values {
		if attachment, _ := value.(string); attachment != "" {
			attachments = append(attachments, attachment)
		}
	}
	return attachments
}

func setVideoResultURL(result *provider.VideoResult, value string) bool {
	value = strings.TrimSpace(value)
	if value == "" {
		return false
	}
	lower := strings.ToLower(value)
	if !strings.HasSuffix(strings.SplitN(lower, "?", 2)[0], ".mp4") && !strings.Contains(lower, "/content") {
		return false
	}
	result.URL = absoluteAssetURL(value)
	result.ContentType = "video/mp4"
	return true
}

func consumeVideoSSE(reader io.Reader, handle func(map[string]any) (bool, error)) error {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64<<10), 8<<20)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if strings.HasPrefix(line, "data:") {
			line = strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		}
		if line == "" || line == "[DONE]" || !strings.HasPrefix(line, "{") {
			continue
		}
		var root map[string]any
		if json.Unmarshal([]byte(line), &root) != nil {
			continue
		}
		complete, err := handle(root)
		if err != nil {
			return err
		}
		if complete {
			return nil
		}
	}
	return scanner.Err()
}

func consumeVideoJSON(reader io.Reader, handle func(map[string]any) (bool, error)) error {
	decoder := json.NewDecoder(io.LimitReader(reader, 64<<20))
	for {
		var root map[string]any
		if err := decoder.Decode(&root); err != nil {
			if err == io.EOF {
				return nil
			}
			return fmt.Errorf("解析视频上游流: %w", err)
		}
		complete, err := handle(root)
		if err != nil {
			return err
		}
		if complete {
			return nil
		}
	}
}

func nestedMap(value map[string]any, keys ...string) map[string]any {
	current := value
	for _, key := range keys {
		next, ok := current[key].(map[string]any)
		if !ok {
			return nil
		}
		current = next
	}
	return current
}

func videoSegments(seconds int) []int {
	if seconds < 1 || seconds > 15 {
		return nil
	}
	return []int{seconds}
}

// videoCreatePayload mirrors the current Grok Imagine browser request.
// In particular, the generation parameters belong in mediaGenInput rather
// than the legacy modelConfigOverride map. Keeping this shape explicit also
// prevents text-to-video from depending on a synthetic media post.
func videoCreatePayload(prompt, ratio, resolution string, seconds int, firstFrameAsset string, referenceAssets []string) map[string]any {
	mediaGenInput := map[string]any{}
	switch {
	case len(referenceAssets) >= 2:
		// Two or more reference assets stay on referenceToVideo. That payload
		// has no custom mode and must not be collapsed into imageToVideo.
		mediaGenInput["referenceToVideo"] = map[string]any{
			"prompt": prompt, "inputAssets": referenceAssets, "aspectRatio": ratio,
			"duration": seconds, "resolutionName": resolution,
		}
	case len(referenceAssets) == 1:
		// A single reference image uses the same imageToVideo shape as a
		// first frame. referenceToVideo does not reliably bind one image.
		mediaGenInput["imageToVideo"] = map[string]any{
			"prompt": prompt, "inputAssets": referenceAssets, "aspectRatio": ratio,
			"duration": seconds, "resolutionName": resolution, "mode": "custom",
		}
	case firstFrameAsset != "":
		// A supplied first frame is an image-to-video request with exactly the
		// first-frame asset and the Web client's custom mode flag.
		mediaGenInput["imageToVideo"] = map[string]any{
			"prompt": prompt, "inputAssets": []string{firstFrameAsset}, "aspectRatio": ratio,
			"duration": seconds, "resolutionName": resolution, "mode": "custom",
		}
	default:
		mediaGenInput["textToVideo"] = map[string]any{
			"prompt": prompt, "aspectRatio": ratio, "duration": seconds, "resolutionName": resolution,
		}
	}
	return map[string]any{
		"modelName":            "imagine-video-gen",
		"message":              prompt + " --mode=custom",
		"enableImageStreaming": true,
		"enableSideBySide":     true,
		"sendFinalMetadata":    true,
		"responseMetadata": map[string]any{
			"experiments": []any{},
			"modelConfigOverride": map[string]any{
				"modelMap": map[string]any{},
			},
		},
		"mediaGenInput": mediaGenInput,
		"kind":          "CONVERSATION_KIND_IMAGINE",
	}
}
