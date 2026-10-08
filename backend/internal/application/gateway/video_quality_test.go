package gateway

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/domain/media"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
)

func TestSaveVideoQualityEvidenceSkipsWhenDisabled(t *testing.T) {
	evidenceRoot := t.TempDir()
	t.Setenv(videoQualityGuardDirEnv, evidenceRoot)
	t.Setenv(videoQualityEvidenceEnabledEnv, "false")

	adapter := &videoQualityFailoverAdapter{}
	service := &Service{}
	service.saveVideoQualityEvidence(
		context.Background(),
		media.Job{ID: "video_disabled_evidence"},
		account.Credential{ID: 101},
		adapter,
		provider.VideoResult{URL: "https://assets.grok.com/rejected.mp4"},
		&videoUpstreamTrace{GenerationAttempt: 1},
		&videoQualityError{reason: "test rejection"},
	)

	if calls := adapter.DownloadCalls(); calls != 0 {
		t.Fatalf("disabled evidence saving downloaded %d videos, want 0", calls)
	}
	if _, err := os.Stat(filepath.Join(evidenceRoot, "rejected-videos")); !os.IsNotExist(err) {
		t.Fatalf("disabled evidence saving created rejected-videos directory, stat err = %v", err)
	}
}

func TestEvaluateUpstreamVideoQualityRejectsMissingModeratedField(t *testing.T) {
	err := evaluateUpstreamVideoQuality(provider.VideoUpstreamMetadata{StreamObserved: true})
	var qualityErr *videoQualityError
	if !errors.As(err, &qualityErr) {
		t.Fatalf("quality error = %v, want videoQualityError", err)
	}
	if !strings.Contains(err.Error(), "moderated") {
		t.Fatalf("quality error = %v, want moderated diagnostic", err)
	}
}

func TestEvaluateUpstreamVideoQualityAcceptsExplicitlyUnmoderatedStream(t *testing.T) {
	err := evaluateUpstreamVideoQuality(provider.VideoUpstreamMetadata{
		StreamObserved:   true,
		ModeratedPresent: true,
		Moderated:        false,
	})
	if err != nil {
		t.Fatalf("explicit moderated:false must pass: %v", err)
	}
}

func TestEvaluateUpstreamVideoQualityRejectsModeratedStream(t *testing.T) {
	err := evaluateUpstreamVideoQuality(provider.VideoUpstreamMetadata{
		StreamObserved:   true,
		ModeratedPresent: true,
		Moderated:        true,
	})
	var qualityErr *videoQualityError
	if !errors.As(err, &qualityErr) {
		t.Fatalf("quality error = %v, want videoQualityError", err)
	}
}

func TestEvaluateUpstreamVideoQualitySkipsProvidersWithoutWebStreamMetadata(t *testing.T) {
	if err := evaluateUpstreamVideoQuality(provider.VideoUpstreamMetadata{}); err != nil {
		t.Fatalf("providers without a Web generation stream must not be rejected: %v", err)
	}
}

func TestVideoQualityCanSwitchAccountExactlySixTimesAfterInitialResult(t *testing.T) {
	for qualityFailures := 1; qualityFailures <= videoQualityAccountSwitches; qualityFailures++ {
		if !videoQualityCanSwitchAccount(qualityFailures) {
			t.Fatalf("quality failure %d must still switch to a new account", qualityFailures)
		}
	}
	if videoQualityCanSwitchAccount(videoQualityAccountSwitches + 1) {
		t.Fatal("the seventh rejected result must stop instead of starting an eighth account")
	}
}

func TestVideoUpstreamLogAttrsIncludesModerationVerdict(t *testing.T) {
	attributes := videoUpstreamLogAttrs(&videoUpstreamTrace{
		ModeratedPresent: true,
		Moderated:        false,
	})
	values := make(map[string]any, len(attributes)/2)
	for index := 0; index+1 < len(attributes); index += 2 {
		key, ok := attributes[index].(string)
		if ok {
			values[key] = attributes[index+1]
		}
	}
	if values["moderated_present"] != true || values["moderated"] != false {
		t.Fatalf("moderation log attributes = %#v", values)
	}
}
