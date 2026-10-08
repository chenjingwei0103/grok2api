package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/domain/media"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
)

const (
	videoQualityGuardDirEnv          = "GROK2API_QUALITY_GUARD_DIR"
	videoQualityEvidenceEnabledEnv   = "GROK2API_SAVE_VIDEO_QUALITY_EVIDENCE"
	videoQualityEvidenceDownloadWait = 2 * time.Minute
)

type videoQualityEvidenceMetadata struct {
	Kind                   string `json:"kind"`
	SavedAt                string `json:"saved_at"`
	JobID                  string `json:"job_id"`
	GenerationAttempt      int    `json:"generation_attempt"`
	AccountID              uint64 `json:"account_id,omitempty"`
	Provider               string `json:"provider,omitempty"`
	Model                  string `json:"model,omitempty"`
	InputMode              string `json:"input_mode,omitempty"`
	ReferenceCount         int    `json:"reference_count"`
	Resolution             string `json:"resolution,omitempty"`
	RequestedAspectRatio   string `json:"requested_aspect_ratio,omitempty"`
	ModeratedPresent       bool   `json:"moderated_present"`
	Moderated              bool   `json:"moderated"`
	QualityRejectionReason string `json:"quality_rejection_reason"`
	ResultURLHost          string `json:"result_url_host,omitempty"`
	ResultURLFingerprint   string `json:"result_url_fingerprint,omitempty"`
	VideoFile              string `json:"video_file,omitempty"`
	ContentType            string `json:"content_type,omitempty"`
	DeclaredBytes          int64  `json:"declared_bytes,omitempty"`
	ActualBytes            int64  `json:"actual_bytes,omitempty"`
	DownloadError          string `json:"download_error,omitempty"`
}

// saveVideoQualityEvidence downloads a quality-rejected result into a private,
// separate evidence directory. It must never use the normal media asset store
// or change the result of the quality decision.
func (s *Service) saveVideoQualityEvidence(
	ctx context.Context,
	job media.Job,
	credential account.Credential,
	adapter provider.VideoAdapter,
	result provider.VideoResult,
	trace *videoUpstreamTrace,
	qualityErr *videoQualityError,
) {
	if !videoQualityEvidenceEnabled() {
		if s != nil && s.logger != nil {
			s.logger.Info("video_quality_evidence_skipped", "job_id", job.ID, "reason", "video evidence saving is disabled")
		}
		return
	}
	root := strings.TrimSpace(os.Getenv(videoQualityGuardDirEnv))
	if root == "" {
		if s != nil && s.logger != nil {
			s.logger.Warn("video_quality_evidence_skipped", "job_id", job.ID, "reason", "quality guard directory is not configured")
		}
		return
	}
	if strings.TrimSpace(result.URL) == "" {
		if s != nil && s.logger != nil {
			s.logger.Warn("video_quality_evidence_skipped", "job_id", job.ID, "reason", "rejected result has no video URL")
		}
		return
	}
	downloader, ok := adapter.(provider.VideoContentDownloader)
	if !ok {
		if s != nil && s.logger != nil {
			s.logger.Warn("video_quality_evidence_skipped", "job_id", job.ID, "reason", "provider does not support video download")
		}
		return
	}

	attempt := 1
	if trace != nil && trace.GenerationAttempt > 0 {
		attempt = trace.GenerationAttempt
	}
	evidenceDir := filepath.Join(root, "rejected-videos", safeVideoEvidenceSegment(job.ID))
	if err := os.MkdirAll(evidenceDir, 0o700); err != nil {
		s.logVideoQualityEvidenceError(job.ID, "create evidence directory", err)
		return
	}

	metadata := videoQualityEvidenceMetadata{
		Kind:                   "grok2api_video_quality_evidence_v1",
		SavedAt:                time.Now().UTC().Format(time.RFC3339Nano),
		JobID:                  job.ID,
		GenerationAttempt:      attempt,
		AccountID:              credential.ID,
		QualityRejectionReason: qualityErrorReason(qualityErr),
	}
	if trace != nil {
		metadata.Provider = trace.Provider
		metadata.Model = trace.Model
		metadata.InputMode = trace.InputMode
		metadata.ReferenceCount = trace.ReferenceCount
		metadata.Resolution = trace.Resolution
		metadata.RequestedAspectRatio = trace.RequestedAspectRatio
		metadata.ModeratedPresent = trace.ModeratedPresent
		metadata.Moderated = trace.Moderated
	}
	metadata.ResultURLHost, metadata.ResultURLFingerprint = videoEvidenceURLFields(result.URL)

	evidenceCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), videoQualityEvidenceDownloadWait)
	defer cancel()
	body, contentType, declaredBytes, downloadErr := downloader.DownloadVideo(evidenceCtx, credential, result.URL)
	metadata.ContentType = contentType
	metadata.DeclaredBytes = declaredBytes
	if downloadErr != nil {
		metadata.DownloadError = downloadErr.Error()
		s.writeVideoQualityEvidenceMetadata(evidenceDir, attempt, metadata)
		s.logVideoQualityEvidenceError(job.ID, "download rejected video", downloadErr)
		return
	}
	if body == nil {
		metadata.DownloadError = "provider returned an empty video body"
		s.writeVideoQualityEvidenceMetadata(evidenceDir, attempt, metadata)
		s.logVideoQualityEvidenceError(job.ID, "download rejected video", fmt.Errorf("empty body"))
		return
	}

	videoPath := filepath.Join(evidenceDir, fmt.Sprintf("attempt-%03d.mp4", attempt))
	temporaryFile, err := os.CreateTemp(evidenceDir, fmt.Sprintf(".attempt-%03d-*.mp4", attempt))
	if err != nil {
		_ = body.Close()
		metadata.DownloadError = err.Error()
		s.writeVideoQualityEvidenceMetadata(evidenceDir, attempt, metadata)
		s.logVideoQualityEvidenceError(job.ID, "create rejected video file", err)
		return
	}
	temporaryPath := temporaryFile.Name()
	actualBytes, copyErr := io.Copy(temporaryFile, body)
	closeErr := body.Close()
	fileCloseErr := temporaryFile.Close()
	if copyErr != nil || closeErr != nil || fileCloseErr != nil {
		err = firstVideoEvidenceError(copyErr, closeErr, fileCloseErr)
		_ = os.Remove(temporaryPath)
		metadata.DownloadError = err.Error()
		metadata.ActualBytes = actualBytes
		s.writeVideoQualityEvidenceMetadata(evidenceDir, attempt, metadata)
		s.logVideoQualityEvidenceError(job.ID, "write rejected video file", err)
		return
	}
	if err := os.Rename(temporaryPath, videoPath); err != nil {
		_ = os.Remove(temporaryPath)
		metadata.DownloadError = err.Error()
		metadata.ActualBytes = actualBytes
		s.writeVideoQualityEvidenceMetadata(evidenceDir, attempt, metadata)
		s.logVideoQualityEvidenceError(job.ID, "finalize rejected video file", err)
		return
	}
	metadata.VideoFile = filepath.Base(videoPath)
	metadata.ActualBytes = actualBytes
	s.writeVideoQualityEvidenceMetadata(evidenceDir, attempt, metadata)
	if s != nil && s.logger != nil {
		s.logger.Info("video_quality_evidence_saved", "job_id", job.ID, "generation_attempt", attempt, "evidence_file", videoPath, "actual_bytes", actualBytes)
	}
}

func videoQualityEvidenceEnabled() bool {
	value := strings.TrimSpace(os.Getenv(videoQualityEvidenceEnabledEnv))
	enabled, err := strconv.ParseBool(value)
	return err == nil && enabled
}

func qualityErrorReason(value *videoQualityError) string {
	if value == nil {
		return "质量校验失败"
	}
	return value.reason
}

func safeVideoEvidenceSegment(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "unknown-job"
	}
	var builder strings.Builder
	for _, runeValue := range value {
		if (runeValue >= 'a' && runeValue <= 'z') || (runeValue >= 'A' && runeValue <= 'Z') || (runeValue >= '0' && runeValue <= '9') || runeValue == '-' || runeValue == '_' || runeValue == '.' {
			builder.WriteRune(runeValue)
		} else {
			builder.WriteByte('_')
		}
	}
	if builder.Len() == 0 {
		return "unknown-job"
	}
	return builder.String()
}

func videoEvidenceURLFields(raw string) (string, string) {
	raw = strings.TrimSpace(raw)
	host := ""
	if parsed, err := url.Parse(raw); err == nil && parsed != nil {
		host = parsed.Hostname()
	}
	digest := sha256.Sum256([]byte(raw))
	return host, hex.EncodeToString(digest[:8])
}

func firstVideoEvidenceError(values ...error) error {
	for _, value := range values {
		if value != nil {
			return value
		}
	}
	return fmt.Errorf("unknown video evidence write error")
}

func (s *Service) writeVideoQualityEvidenceMetadata(directory string, attempt int, metadata videoQualityEvidenceMetadata) {
	path := filepath.Join(directory, fmt.Sprintf("attempt-%03d.json", attempt))
	data, err := json.MarshalIndent(metadata, "", "  ")
	if err != nil {
		s.logVideoQualityEvidenceError(metadata.JobID, "encode evidence metadata", err)
		return
	}
	temporaryFile, err := os.CreateTemp(directory, fmt.Sprintf(".attempt-%03d-*.json", attempt))
	if err != nil {
		s.logVideoQualityEvidenceError(metadata.JobID, "create evidence metadata", err)
		return
	}
	temporaryPath := temporaryFile.Name()
	if _, err = temporaryFile.Write(data); err == nil {
		err = temporaryFile.Close()
	} else {
		_ = temporaryFile.Close()
	}
	if err == nil {
		err = os.Rename(temporaryPath, path)
	}
	if err != nil {
		_ = os.Remove(temporaryPath)
		s.logVideoQualityEvidenceError(metadata.JobID, "write evidence metadata", err)
	}
}

func (s *Service) logVideoQualityEvidenceError(jobID, action string, err error) {
	if s != nil && s.logger != nil && err != nil {
		s.logger.Warn("video_quality_evidence_failed", "job_id", jobID, "action", action, "error", err.Error())
	}
}
