package cli

import (
	"encoding/json"
	"strconv"
	"strings"
	"sync"

	"github.com/google/uuid"
	"github.com/klauspost/compress/zstd"
)

const (
	grokConversationGroupNamespace  = "xai:grok-build:conversation-group:"
	buildRequestCompressionMinBytes = 64 << 10
	grokDoomLoopCheck               = "1024"
	grokExactRepetitionCheck        = "64"
	grokCompactionsRemaining        = "1"
	grokCompactionAt                = "400000"
)

var (
	buildZstdOnce    sync.Once
	buildZstdEncoder *zstd.Encoder
)

// grokConversationGroupID matches xai-grok-shell derive_conversation_group_id.
func grokConversationGroupID(rootSessionID string) string {
	rootSessionID = strings.TrimSpace(rootSessionID)
	if rootSessionID == "" {
		return ""
	}
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte(grokConversationGroupNamespace+rootSessionID)).String()
}

func applyGrokCLIFidelityHeaders(header interface{ Set(string, string) }) {
	header.Set("x-grok-doom-loop-check", grokDoomLoopCheck)
	header.Set("x-grok-exact-repetition-check", grokExactRepetitionCheck)
	header.Set("x-compactions-remaining", grokCompactionsRemaining)
	header.Set("x-compaction-at", grokCompactionAt)
}

// grokTurnIndexForRequest keeps an explicit client turn. When the client omits
// it but the request already has a stable session, the index is the number of
// user turns in the normalized body, matching Grok CLI 1.0.46.
func grokTurnIndexForRequest(explicit, sessionID string, body []byte) string {
	if turn := normalizeGrokTurnIndex(explicit); turn != "" {
		return turn
	}
	if strings.TrimSpace(sessionID) == "" || len(body) == 0 {
		return ""
	}
	return strconv.Itoa(estimateGrokTurnIndex(body))
}

func estimateGrokTurnIndex(body []byte) int {
	var payload struct {
		Input []struct {
			Type string `json:"type"`
			Role string `json:"role"`
		} `json:"input"`
		Messages []struct {
			Role string `json:"role"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return 1
	}
	count := 0
	if len(payload.Input) == 0 {
		for _, item := range payload.Messages {
			if strings.EqualFold(item.Role, "user") {
				count++
			}
		}
	} else {
		for _, item := range payload.Input {
			if strings.EqualFold(item.Role, "user") && (item.Type == "" || strings.EqualFold(item.Type, "message")) {
				count++
			}
		}
	}
	if count < 1 {
		return 1
	}
	return count
}

func compressBuildPlaneRequest(plane string, body []byte) ([]byte, string) {
	if plane != "build" || len(body) < buildRequestCompressionMinBytes {
		return body, ""
	}
	encoder := buildRequestZstdEncoder()
	if encoder == nil {
		return body, ""
	}
	compressed := encoder.EncodeAll(body, nil)
	if len(compressed) == 0 || len(compressed) >= len(body) {
		return body, ""
	}
	return compressed, "zstd"
}

func buildRequestZstdEncoder() *zstd.Encoder {
	buildZstdOnce.Do(func() {
		encoder, err := zstd.NewWriter(nil)
		if err == nil {
			buildZstdEncoder = encoder
		}
	})
	return buildZstdEncoder
}
