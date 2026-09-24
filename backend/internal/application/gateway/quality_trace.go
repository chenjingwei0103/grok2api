package gateway

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/domain/audit"
	infraegress "github.com/chenyme/grok2api/backend/internal/infra/egress"
)

const qualityTraceEnvelopeLimit = 48 << 10

// qualityTextCapture retains only visible output. It deliberately never sees
// raw SSE or encrypted reasoning payloads.
type qualityTextCapture struct {
	limit       int
	value       []byte
	sourceBytes int
	truncated   bool
}

func newQualityTextCapture(limit int) *qualityTextCapture {
	if limit <= 0 {
		limit = defaultQualityTraceOutputBytes
	}
	return &qualityTextCapture{limit: limit}
}

func (c *qualityTextCapture) appendText(value string) {
	if c == nil || value == "" {
		return
	}
	c.sourceBytes += len(value)
	remaining := c.limit - len(c.value)
	if remaining <= 0 {
		c.truncated = true
		return
	}
	part := truncateDiagnosticText(value, remaining)
	c.value = append(c.value, part...)
	if len(part) < len(value) {
		c.truncated = true
	}
}

func (c *qualityTextCapture) clone() *qualityTextCapture {
	if c == nil {
		return nil
	}
	return &qualityTextCapture{
		limit:       c.limit,
		value:       append([]byte(nil), c.value...),
		sourceBytes: c.sourceBytes,
		truncated:   c.truncated,
	}
}

func (c *qualityTextCapture) text() string {
	if c == nil || len(c.value) == 0 {
		return ""
	}
	return string(c.value)
}

func (s qualityScanState) clone() qualityScanState {
	copy := s
	copy.pending = append([]byte(nil), s.pending...)
	copy.visibleOutput = s.visibleOutput.clone()
	return copy
}

// qualityStreamCapture is the non-secret scanner snapshot retained for an
// audit trace. Held bytes are counted but never saved as raw SSE.
type qualityStreamCapture struct {
	protocol      string
	state         qualityScanState
	heldBytes     int
	heldTruncated bool
}

func newQualityStreamCapture(protocol string, state qualityScanState, heldBytes int, heldTruncated bool) qualityStreamCapture {
	return qualityStreamCapture{
		protocol: protocol, state: state.clone(), heldBytes: heldBytes, heldTruncated: heldTruncated,
	}
}

func (c qualityStreamCapture) clone() qualityStreamCapture {
	return newQualityStreamCapture(c.protocol, c.state, c.heldBytes, c.heldTruncated)
}

func (c qualityStreamCapture) signals() QualityStreamSignals {
	state := c.state.clone()
	return state.signals()
}

// qualityTraceReadCloser observes the part of a delivered stream that was not
// already consumed by the quality hold. The held prefix is skipped because it
// is already present in the copied scanner state.
type qualityTraceReadCloser struct {
	source        io.ReadCloser
	state         qualityScanState
	protocol      string
	skip          int
	heldBytes     int
	heldTruncated bool
	once          sync.Once
	onFinish      func(qualityStreamCapture)
}

func newQualityTraceReadCloser(source io.ReadCloser, initial qualityStreamCapture, onFinish func(qualityStreamCapture)) *qualityTraceReadCloser {
	if source == nil {
		source = io.NopCloser(bytes.NewReader(nil))
	}
	return &qualityTraceReadCloser{
		source: source, state: initial.state.clone(), protocol: initial.protocol,
		skip: initial.heldBytes, heldBytes: initial.heldBytes, heldTruncated: initial.heldTruncated, onFinish: onFinish,
	}
}

func (r *qualityTraceReadCloser) Read(dst []byte) (int, error) {
	n, err := r.source.Read(dst)
	if n > 0 {
		chunk := dst[:n]
		if r.skip > 0 {
			skipped := min(r.skip, len(chunk))
			r.skip -= skipped
			chunk = chunk[skipped:]
		}
		if len(chunk) > 0 {
			ObserveQualityChunk(&r.state, chunk)
		}
	}
	if err != nil {
		if err == io.EOF {
			r.state.streamEOF = true
			r.state.markTerminal()
		}
		r.finish()
	}
	return n, err
}

func (r *qualityTraceReadCloser) Close() error {
	r.finish()
	return r.source.Close()
}

func (r *qualityTraceReadCloser) capture() qualityStreamCapture {
	if r == nil {
		return qualityStreamCapture{}
	}
	return newQualityStreamCapture(r.protocol, r.state, r.heldBytes, r.heldTruncated)
}

func (r *qualityTraceReadCloser) finish() {
	if r == nil {
		return
	}
	r.once.Do(func() {
		if r.onFinish == nil {
			return
		}
		r.onFinish(r.capture())
	})
}

type qualityTraceRequest struct {
	SHA256    string
	Bytes     int
	Preview   string
	Truncated bool
	Redacted  bool
}

func newQualityTraceRequest(body []byte, limit int) qualityTraceRequest {
	digest := sha256.Sum256(body)
	result := qualityTraceRequest{SHA256: fmt.Sprintf("%x", digest[:]), Bytes: len(body)}
	if len(body) == 0 || limit <= 0 {
		return result
	}
	preview := ""
	redacted := false
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err == nil {
		var trailing any
		if err := decoder.Decode(&trailing); err == io.EOF {
			value, redacted = sanitizeQualityTraceJSONValue(value)
			if encoded, marshalErr := json.Marshal(value); marshalErr == nil {
				preview = string(encoded)
			}
		}
	}
	if preview == "" {
		preview = sanitizeDiagnosticText(string(body), limit)
	} else {
		preview = sanitizeDiagnosticText(preview, limit)
	}
	result.Preview = preview
	result.Truncated = len(preview) < len(body)
	result.Redacted = redacted
	return result
}

func sanitizeQualityTraceJSONValue(value any) (any, bool) {
	switch current := value.(type) {
	case map[string]any:
		result := make(map[string]any, len(current))
		redacted := false
		for key, nested := range current {
			if isSensitiveQualityTraceKey(key) {
				result[key] = "[REDACTED]"
				redacted = true
				continue
			}
			clean, nestedRedacted := sanitizeQualityTraceJSONValue(nested)
			result[key] = clean
			redacted = redacted || nestedRedacted
		}
		return result, redacted
	case []any:
		result := make([]any, 0, len(current))
		redacted := false
		for _, nested := range current {
				clean, nestedRedacted := sanitizeQualityTraceJSONValue(nested)
				result = append(result, clean)
				redacted = redacted || nestedRedacted
		}
		return result, redacted
	case string:
		if replacement, redacted := redactQualityTraceBlob(current); redacted {
			return replacement, true
		}
		return current, false
	default:
		return value, false
	}
}

func isSensitiveQualityTraceKey(value string) bool {
	normalized := strings.NewReplacer("-", "", "_", "", ".", "", " ", "").Replace(strings.ToLower(strings.TrimSpace(value)))
	if normalized == "key" || normalized == "signature" {
		return true
	}
	for _, marker := range []string{
		"authorization", "cookie", "credential", "password", "secret", "token", "apikey", "accesskey", "privatekey", "session", "encryptedcontent",
	} {
		if strings.Contains(normalized, marker) {
			return true
		}
	}
	return false
}

func redactQualityTraceBlob(value string) (string, bool) {
	trimmed := strings.TrimSpace(value)
	if strings.HasPrefix(strings.ToLower(trimmed), "data:") {
		return fmt.Sprintf("[REDACTED_DATA_URL bytes=%d]", len(value)), true
	}
	if len(trimmed) < 1024 {
		return "", false
	}
	base64Chars := 0
	for _, char := range trimmed {
		switch {
		case char >= 'a' && char <= 'z', char >= 'A' && char <= 'Z', char >= '0' && char <= '9', char == '+', char == '/', char == '=', char == '-', char == '_', char == '\r', char == '\n':
			base64Chars++
		default:
			return "", false
		}
	}
	if base64Chars == utf8.RuneCountInString(trimmed) {
		return fmt.Sprintf("[REDACTED_BINARY bytes=%d]", len(value)), true
	}
	return "", false
}

type qualityTraceEgress struct {
	ConfiguredNodeID uint64 `json:"configuredNodeId,omitempty"`
	ActualNodeID     uint64 `json:"actualNodeId,omitempty"`
	ActualNodeName   string `json:"actualNodeName,omitempty"`
	Scope            string `json:"scope,omitempty"`
	Mode             string `json:"mode,omitempty"`
}

func snapshotQualityTraceEgress(credential accountdomain.Credential, trace *infraegress.Trace, provider accountdomain.Provider) qualityTraceEgress {
	result := qualityTraceEgress{ConfiguredNodeID: credential.EgressNodeID}
	if trace == nil {
		return result
	}
	selection, ok := trace.Selection(primaryEgressScope(provider))
	if !ok {
		return result
	}
	result.ActualNodeID = selection.NodeID
	result.ActualNodeName = selection.NodeName
	result.Scope = string(selection.Scope)
	if selection.Proxied {
		result.Mode = string(audit.EgressModeProxy)
	} else {
		result.Mode = string(audit.EgressModeDirect)
	}
	return result
}

type qualityTraceAttemptInput struct {
	Request         qualityTraceRequest
	RequestID       string
	RequestPath     string
	PublicModel     string
	UpstreamModel   string
	Provider        string
	Operation       audit.Operation
	ReasoningEffort string
	ClientSource    string
	QualityAttempt  int
	Verdict         QualityVerdict
	Action          string
	Retry           QualityRetryRuntime
	Capture         qualityStreamCapture
	Egress          qualityTraceEgress
	UpstreamURL     string
}

func (value qualityTraceAttemptInput) withCapture(capture qualityStreamCapture) qualityTraceAttemptInput {
	value.Capture = capture
	return value
}

func qualityTraceClientSource(headers map[string][]string) string {
	for name, values := range headers {
		if !strings.EqualFold(strings.TrimSpace(name), "X-Client-Source") || len(values) == 0 {
			continue
		}
		return sanitizeDiagnosticText(values[0], 128)
	}
	return ""
}

func (value qualityTraceAttemptInput) qualityError() string {
	if value.Verdict != QualityWithhold {
		return ""
	}
	reasons := qualityTraceReasons(value.Capture.signals(), value.Retry, value.Verdict)
	if len(reasons) == 0 {
		return ErrorQualityDegraded
	}
	return ErrorQualityDegraded + ":" + strings.Join(reasons, ",")
}

type qualityTraceEnvelope struct {
	Kind    string `json:"kind"`
	Request struct {
		ID       string `json:"id"`
		Method   string `json:"method"`
		Path     string `json:"path"`
		Streaming bool  `json:"streaming"`
		Body     struct {
			SHA256    string `json:"sha256"`
			Bytes     int    `json:"bytes"`
			Preview   string `json:"preview"`
			Truncated bool   `json:"truncated"`
			Redacted  bool   `json:"redacted"`
		} `json:"body"`
	} `json:"request"`
	Route struct {
		PublicModel     string `json:"publicModel"`
		UpstreamModel   string `json:"upstreamModel"`
		Provider        string `json:"provider"`
		Operation       string `json:"operation"`
		ReasoningEffort string `json:"reasoningEffort,omitempty"`
		ClientSource    string `json:"clientSource,omitempty"`
	} `json:"route"`
	Account struct {
		ID   uint64 `json:"id,omitempty"`
		Name string `json:"name,omitempty"`
	} `json:"account"`
	Egress qualityTraceEgress `json:"egress"`
	Quality struct {
		Attempt    int      `json:"attempt"`
		Verdict    string   `json:"verdict"`
		Action     string   `json:"action"`
		Reasons    []string `json:"reasons"`
		Thresholds struct {
			MaxOutputTokensPerSecond float64 `json:"maxOutputTokensPerSecond"`
			MinOutputTokens          int64   `json:"minOutputTokens"`
			HoldTimeoutMS            int64   `json:"holdTimeoutMs"`
		} `json:"thresholds"`
		Signals struct {
			HasThinking           bool    `json:"hasThinking"`
			PlaintextThinking     bool    `json:"plaintextThinking"`
			ReasoningStarted      bool    `json:"reasoningStarted"`
			VisibleTokens         int64   `json:"visibleTokens"`
			OutputTokens          int64   `json:"outputTokens"`
			ReasoningTokens       int64   `json:"reasoningTokens"`
			EncryptedBytes        int     `json:"encryptedBytes"`
			FirstVisible          bool    `json:"firstVisible"`
			VisibleFlushMS        int64   `json:"visibleFlushMs"`
			Terminal              bool    `json:"terminal"`
			HoldExpired           bool    `json:"holdExpired"`
			ClassifierTPS         float64 `json:"classifierOutputTokensPerSecond"`
			FullRequestTPS        float64 `json:"fullRequestOutputTokensPerSecond"`
		} `json:"signals"`
	} `json:"quality"`
	Timing struct {
		HoldStartedAt    time.Time `json:"holdStartedAt,omitempty"`
		FirstGeneratedMS int64     `json:"firstGeneratedMs,omitempty"`
		FirstVisibleMS   int64     `json:"firstVisibleMs,omitempty"`
		CompletedMS      int64     `json:"completedMs,omitempty"`
		StreamEOF        bool      `json:"streamEof"`
		HeldBytes        int       `json:"heldBytes"`
		HeldTruncated    bool      `json:"heldTruncated"`
	} `json:"timing"`
	Usage struct {
		Reported      bool  `json:"reported"`
		InputTokens   int64 `json:"inputTokens"`
		OutputTokens  int64 `json:"outputTokens"`
		ReasoningTokens int64 `json:"reasoningTokens"`
		TotalTokens   int64 `json:"totalTokens"`
	} `json:"usage"`
	Output struct {
		VisibleText  string `json:"visibleText"`
		SourceBytes  int    `json:"sourceBytes"`
		Truncated    bool   `json:"truncated"`
	} `json:"output"`
	Truncated bool `json:"truncated"`
}

func newQualityTraceEnvelope(input qualityTraceAttemptInput, credential accountdomain.Credential) qualityTraceEnvelope {
	capture := input.Capture.clone()
	signals := capture.signals()
	state := capture.state
	visible := state.visibleOutput
	if visible == nil {
		visible = newQualityTextCapture(input.Retry.Trace.OutputMaxBytes)
	}
	result := qualityTraceEnvelope{Kind: "quality_trace_v1"}
	result.Request.ID = input.RequestID
	result.Request.Method = "POST"
	result.Request.Path = sanitizeRequestPath(input.RequestPath)
	result.Request.Streaming = true
	result.Request.Body.SHA256 = input.Request.SHA256
	result.Request.Body.Bytes = input.Request.Bytes
	result.Request.Body.Preview = input.Request.Preview
	result.Request.Body.Truncated = input.Request.Truncated
	result.Request.Body.Redacted = input.Request.Redacted
	result.Route.PublicModel = input.PublicModel
	result.Route.UpstreamModel = input.UpstreamModel
	result.Route.Provider = input.Provider
	result.Route.Operation = string(input.Operation)
	result.Route.ReasoningEffort = input.ReasoningEffort
	result.Route.ClientSource = input.ClientSource
	result.Account.ID = credential.ID
	result.Account.Name = credential.Name
	result.Egress = input.Egress
	result.Quality.Attempt = input.QualityAttempt
	result.Quality.Verdict = string(input.Verdict)
	result.Quality.Action = input.Action
	result.Quality.Reasons = qualityTraceReasons(signals, input.Retry, input.Verdict)
	result.Quality.Thresholds.MaxOutputTokensPerSecond = input.Retry.MaxOutputTokensPerSecond
	result.Quality.Thresholds.MinOutputTokens = input.Retry.MinOutputTokens
	result.Quality.Thresholds.HoldTimeoutMS = input.Retry.HoldTimeout.Milliseconds()
	result.Quality.Signals.HasThinking = signals.HasThinking
	result.Quality.Signals.PlaintextThinking = signals.PlaintextThinking
	result.Quality.Signals.ReasoningStarted = signals.ReasoningStarted
	result.Quality.Signals.VisibleTokens = signals.VisibleTokens
	result.Quality.Signals.OutputTokens = signals.OutputTokens
	result.Quality.Signals.ReasoningTokens = signals.ReasoningTokens
	result.Quality.Signals.EncryptedBytes = signals.EncryptedBytes
	result.Quality.Signals.FirstVisible = signals.FirstVisible
	result.Quality.Signals.VisibleFlushMS = signals.VisibleFlushMS
	result.Quality.Signals.Terminal = signals.Terminal
	result.Quality.Signals.HoldExpired = signals.HoldExpired
	result.Quality.Signals.ClassifierTPS = signals.OutputTokensPerSecond
	if !state.startedAt.IsZero() && !state.completedAt.IsZero() && signals.OutputTokens > 0 {
		durationMS := state.completedAt.Sub(state.startedAt).Milliseconds()
		if durationMS > 0 {
			result.Quality.Signals.FullRequestTPS = float64(signals.OutputTokens) * 1000 / float64(durationMS)
		}
	}
	result.Timing.HoldStartedAt = state.startedAt.UTC()
	result.Timing.FirstGeneratedMS = elapsedMilliseconds(state.startedAt, state.firstGeneratedAt)
	result.Timing.FirstVisibleMS = elapsedMilliseconds(state.startedAt, state.firstVisibleAt)
	result.Timing.CompletedMS = elapsedMilliseconds(state.startedAt, state.completedAt)
	result.Timing.StreamEOF = state.streamEOF
	result.Timing.HeldBytes = capture.heldBytes
	result.Timing.HeldTruncated = capture.heldTruncated
	result.Usage.Reported = state.usage.Reported
	result.Usage.InputTokens = state.usage.InputTokens
	result.Usage.OutputTokens = signals.OutputTokens
	result.Usage.ReasoningTokens = signals.ReasoningTokens
	result.Usage.TotalTokens = state.usage.TotalTokens
	result.Output.VisibleText = sanitizeDiagnosticText(visible.text(), input.Retry.Trace.OutputMaxBytes)
	result.Output.SourceBytes = visible.sourceBytes
	result.Output.Truncated = visible.truncated || len(result.Output.VisibleText) < len(visible.text())
	return result
}

func elapsedMilliseconds(startedAt, value time.Time) int64 {
	if startedAt.IsZero() || value.IsZero() {
		return 0
	}
	return max(int64(0), value.Sub(startedAt).Milliseconds())
}

func qualityTraceReasons(sig QualityStreamSignals, retry QualityRetryRuntime, verdict QualityVerdict) []string {
	if verdict == QualityWait {
		return []string{"awaiting_more_evidence"}
	}
	if retry.MaxOutputTokensPerSecond > 0 && sig.Terminal && sig.OutputTokensPerSecond > retry.MaxOutputTokensPerSecond {
		return []string{"tps_exceeded"}
	}
	if verdict == QualityWithhold {
		reasons := make([]string, 0, 2)
		if qualityIsBurstDump(sig, retry.MinOutputTokens) {
			reasons = append(reasons, "burst_dump")
		}
		if qualityIsFakeEncryptedDump(sig, retry.MinOutputTokens) {
			reasons = append(reasons, "fake_encrypted_dump")
		}
		if qualityIsFastReasoningRatioDump(sig) {
			reasons = append(reasons, "fast_reasoning_ratio_dump")
		}
		if qualityIsCipherDrool(sig, retry.MinOutputTokens) {
			reasons = append(reasons, "cipher_drool")
		}
		if len(reasons) == 0 {
			reasons = append(reasons, "missing_reasoning")
		}
		return reasons
	}
	if sig.PlaintextThinking {
		return []string{"plaintext_reasoning"}
	}
	if sig.HasThinking {
		return []string{"encrypted_reasoning_evidence"}
	}
	return []string{"below_quality_hold_threshold"}
}

func marshalQualityTraceEnvelope(value qualityTraceEnvelope) ([]byte, bool) {
	for {
		encoded, err := json.Marshal(value)
		if err != nil {
			return []byte(`{"kind":"quality_trace_v1","truncated":true}`), true
		}
		if len(encoded) <= qualityTraceEnvelopeLimit {
			return encoded, value.Truncated
		}
		value.Truncated = true
		switch {
		case value.Output.VisibleText != "":
			value.Output.VisibleText = truncateDiagnosticText(value.Output.VisibleText, max(1, len(value.Output.VisibleText)/2))
			value.Output.Truncated = true
		case value.Request.Body.Preview != "":
			value.Request.Body.Preview = truncateDiagnosticText(value.Request.Body.Preview, max(1, len(value.Request.Body.Preview)/2))
			value.Request.Body.Truncated = true
		default:
			return []byte(`{"kind":"quality_trace_v1","truncated":true}`), true
		}
	}
}
