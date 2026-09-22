package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/chenyme/grok2api/backend/internal/domain/audit"
)

const (
	qualityProtocolChat                = "chat"
	qualityProtocolResponses           = "responses"
	qualityProtocolAnthropic           = "anthropic"
	qualityReasoningSSEComment         = ": grok2api-reasoning-start"
	qualityReasoningEvidenceSSEComment = ": grok2api-reasoning-evidence"
	qualityHoldMaxBufferBytes          = 4 << 20
)

type qualityScanState struct {
	protocol                        string
	pending                         []byte
	hasThinking                     bool
	reasoningStarted                bool
	visibleRunes                    int
	aggregateRunes                  int
	semanticOutput                  bool
	reasoningTokens                 int64
	outputTokens                    int64
	encryptedBytes                  int
	minEncryptedBytes               int
	encryptedBytesPerReasoningToken int
	usage                           Usage
	responseID                      string
	terminal                        bool
	streamEOF                       bool
	holdExpired                     bool
	startedAt                       time.Time
	firstGeneratedAt                time.Time
	completedAt                     time.Time
	firstVisibleAt                  time.Time
	// visibleOutput is enabled only for private quality diagnostics. It holds
	// user-visible text, never raw SSE or reasoning ciphertext.
	visibleOutput *qualityTextCapture
}

type qualityReadResult struct {
	data []byte
	err  error
}

// qualityReadPump is the sole reader of the upstream body. It lets the hold
// timer win while an upstream Read is blocked, then remains the continuation
// reader for the response body after the held prefix is replayed.
type qualityReadPump struct {
	source    io.ReadCloser
	results   chan qualityReadResult
	done      chan struct{}
	closeOnce sync.Once
	pending   []byte
	finalErr  error
}

func newQualityReadPump(source io.ReadCloser) *qualityReadPump {
	pump := &qualityReadPump{
		source:  source,
		results: make(chan qualityReadResult),
		done:    make(chan struct{}),
	}
	go pump.run()
	return pump
}

func (p *qualityReadPump) run() {
	defer close(p.results)
	buf := make([]byte, 4096)
	for {
		n, err := p.source.Read(buf)
		if n == 0 && err == nil {
			continue
		}
		result := qualityReadResult{err: err}
		if n > 0 {
			result.data = append([]byte(nil), buf[:n]...)
		}
		select {
		case p.results <- result:
		case <-p.done:
			return
		}
		if err != nil {
			return
		}
	}
}

func (p *qualityReadPump) Read(dst []byte) (int, error) {
	for len(p.pending) == 0 {
		if p.finalErr != nil {
			return 0, p.finalErr
		}
		result, ok := <-p.results
		if !ok {
			p.finalErr = io.EOF
			return 0, io.EOF
		}
		p.pending = result.data
		p.finalErr = result.err
		if len(p.pending) == 0 && p.finalErr != nil {
			return 0, p.finalErr
		}
	}
	n := copy(dst, p.pending)
	p.pending = p.pending[n:]
	return n, nil
}

func (p *qualityReadPump) Close() error {
	var err error
	p.closeOnce.Do(func() {
		close(p.done)
		err = p.source.Close()
	})
	return err
}

func qualityProtocolForOperation(operation audit.Operation) string {
	switch operation {
	case audit.OperationChat:
		return qualityProtocolChat
	case audit.OperationMessages:
		return qualityProtocolAnthropic
	default:
		return qualityProtocolResponses
	}
}

func (s *qualityScanState) signals() QualityStreamSignals {
	// Visible tokens come only from streamed content deltas (chat content /
	// responses output_text / message items). Do not lift from
	// usage.output − usage.reasoning: chat often reports completion_tokens
	// that still include reasoning, which made dumps look like long answers.
	visibleRunes := max(s.visibleRunes, s.aggregateRunes)
	visible := int64((visibleRunes + 3) / 4)
	output := s.outputTokens
	if s.usage.Reported && s.usage.OutputTokens > output {
		output = s.usage.OutputTokens
	}
	// Usage.reasoning_tokens is not proof of thinking. 降智 accounts report
	// hundreds of reasoning tokens on completed while the stream never sent
	// reasoning_text / reasoning_summary deltas (TUI shows no thoughts).
	// A non-empty encrypted_content stub is also not thinking until it meets
	// the ciphertext floor (default 256 bytes, or 4 bytes per reasoning token).
	reasoningTokens := max(s.reasoningTokens, s.usage.ReasoningTokens)
	floor := encryptedThinkingFloor(s.minEncryptedBytes, s.encryptedBytesPerReasoningToken, reasoningTokens)
	hasThinking := s.hasThinking || s.encryptedBytes >= floor
	firstVisible := !s.firstVisibleAt.IsZero()
	var flushMS int64
	if firstVisible {
		flushMS = time.Since(s.firstVisibleAt).Milliseconds()
	}
	outputTokensForSpeed := output
	if outputTokensForSpeed <= 0 {
		outputTokensForSpeed = visible + reasoningTokens
	}
	var outputTokensPerSecond float64
	if outputTokensForSpeed > 0 && !s.startedAt.IsZero() && !s.firstGeneratedAt.IsZero() && !s.completedAt.IsZero() {
		firstTokenMS := s.firstGeneratedAt.Sub(s.startedAt).Milliseconds()
		durationMS := s.completedAt.Sub(s.startedAt).Milliseconds()
		// Fully buffered responses can have equal millisecond timestamps.
		// Keep the denominator positive so a genuine short burst is measured.
		if durationMS <= firstTokenMS {
			durationMS = firstTokenMS + 1
		}
		outputTokensPerSecond = audit.OutputTokensPerSecond(outputTokensForSpeed, reasoningTokens, firstTokenMS, durationMS)
	}
	s.usage.OutputTokensPerSecond = outputTokensPerSecond
	return QualityStreamSignals{
		HasThinking:           hasThinking,
		PlaintextThinking:     s.hasThinking,
		ReasoningStarted:      s.reasoningStarted || hasThinking,
		VisibleTokens:         visible,
		ReasoningTokens:       reasoningTokens,
		OutputTokens:          output,
		EncryptedBytes:        s.encryptedBytes,
		FirstVisible:          firstVisible,
		VisibleFlushMS:        flushMS,
		Terminal:              s.terminal,
		HoldExpired:           s.holdExpired,
		OutputTokensPerSecond: outputTokensPerSecond,
	}
}

func (s *qualityScanState) markGenerated() {
	if s == nil || !s.firstGeneratedAt.IsZero() {
		return
	}
	s.firstGeneratedAt = time.Now()
}

func (s *qualityScanState) markTerminal() {
	if s == nil {
		return
	}
	s.terminal = true
	if s.completedAt.IsZero() {
		s.completedAt = time.Now()
	}
}

// ObserveQualityChunk feeds one SSE chunk into the hold classifier state.
// This is the shipped scanner used by peekQualityStream.
func ObserveQualityChunk(state *qualityScanState, chunk []byte) {
	if state == nil || len(chunk) == 0 {
		return
	}
	state.pending = append(state.pending, chunk...)
	for {
		index := bytes.IndexByte(state.pending, '\n')
		if index < 0 {
			if len(state.pending) > 1<<20 {
				state.pending = nil
			}
			return
		}
		line := bytes.TrimSpace(state.pending[:index])
		state.pending = state.pending[index+1:]
		if len(line) == 0 {
			continue
		}
		if bytes.Equal(line, []byte(qualityReasoningSSEComment)) {
			// Timing stub only. 降智 still emits this, then usage.reasoning_tokens=0.
			state.markGenerated()
			state.reasoningStarted = true
			continue
		}
		if bytes.Equal(line, []byte(qualityReasoningEvidenceSSEComment)) {
			// Protocol converters cannot expose encrypted_content in every public
			// JSON contract. This internal SSE comment preserves that evidence.
			state.markGenerated()
			state.reasoningStarted = true
			state.hasThinking = true
			continue
		}
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		payload := bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
		if bytes.Equal(payload, []byte("[DONE]")) {
			state.markTerminal()
			continue
		}
		observeQualityPayload(state, payload)
	}
}

func observeQualityPayload(state *qualityScanState, payload []byte) {
	switch state.protocol {
	case qualityProtocolChat:
		observeQualityChat(state, payload)
	case qualityProtocolAnthropic:
		observeQualityAnthropic(state, payload)
	default:
		observeQualityResponses(state, payload)
	}
}

func observeQualityChat(state *qualityScanState, payload []byte) {
	var event struct {
		ID      string `json:"id"`
		Model   string `json:"model"`
		Choices []struct {
			Delta struct {
				Content          string `json:"content"`
				Reasoning        string `json:"reasoning"`
				ReasoningContent string `json:"reasoning_content"`
				ThinkingContent  string `json:"thinking_content"`
				ToolCalls        []any  `json:"tool_calls"`
				FunctionCall     any    `json:"function_call"`
			} `json:"delta"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage *struct {
			PromptTokens            int64 `json:"prompt_tokens"`
			CompletionTokens        int64 `json:"completion_tokens"`
			TotalTokens             int64 `json:"total_tokens"`
			CompletionTokensDetails struct {
				ReasoningTokens int64 `json:"reasoning_tokens"`
			} `json:"completion_tokens_details"`
		} `json:"usage"`
	}
	if json.Unmarshal(payload, &event) != nil {
		return
	}
	if state.responseID == "" {
		state.responseID = event.ID
	}
	if event.Usage != nil {
		state.usage.Reported = true
		state.usage.InputTokens = event.Usage.PromptTokens
		state.usage.OutputTokens = event.Usage.CompletionTokens
		state.usage.ReasoningTokens = event.Usage.CompletionTokensDetails.ReasoningTokens
		state.usage.TotalTokens = event.Usage.TotalTokens
		state.usage.ResponseModel = event.Model
		state.outputTokens = event.Usage.CompletionTokens
		state.reasoningTokens = event.Usage.CompletionTokensDetails.ReasoningTokens
	}
	for _, choice := range event.Choices {
		delta := choice.Delta
		if strings.TrimSpace(delta.Reasoning) != "" || strings.TrimSpace(delta.ReasoningContent) != "" || strings.TrimSpace(delta.ThinkingContent) != "" {
			state.markGenerated()
			state.hasThinking = true
		}
		if delta.Content != "" {
			noteVisibleContent(state, delta.Content)
		}
		if len(delta.ToolCalls) > 0 || delta.FunctionCall != nil {
			state.markGenerated()
			state.semanticOutput = true
		}
		if choice.FinishReason != "" {
			state.markTerminal()
		}
	}
}

type qualityResponsesOutputItem struct {
	ID               string `json:"id"`
	Type             string `json:"type"`
	EncryptedContent string `json:"encrypted_content"`
	Content          []struct {
		Type    string `json:"type"`
		Text    string `json:"text"`
		Refusal string `json:"refusal"`
	} `json:"content"`
}

func noteEncryptedBytes(state *qualityScanState, blob string) {
	if n := len(strings.TrimSpace(blob)); n > state.encryptedBytes {
		state.encryptedBytes = n
	}
}

func noteResponsesReasoningItem(state *qualityScanState, item qualityResponsesOutputItem) {
	if !strings.EqualFold(strings.TrimSpace(item.Type), "reasoning") {
		return
	}
	if strings.TrimSpace(item.ID) != "" {
		state.markGenerated()
		state.reasoningStarted = true
	}
	noteEncryptedBytes(state, item.EncryptedContent)
}

func observeQualityResponses(state *qualityScanState, payload []byte) {
	var event struct {
		Type     string                     `json:"type"`
		Delta    string                     `json:"delta"`
		Item     qualityResponsesOutputItem `json:"item"`
		Response *struct {
			ID     string                       `json:"id"`
			Model  string                       `json:"model"`
			Output []qualityResponsesOutputItem `json:"output"`
			Usage  *struct {
				OutputTokens        int64 `json:"output_tokens"`
				InputTokens         int64 `json:"input_tokens"`
				TotalTokens         int64 `json:"total_tokens"`
				OutputTokensDetails struct {
					ReasoningTokens int64 `json:"reasoning_tokens"`
				} `json:"output_tokens_details"`
			} `json:"usage"`
		} `json:"response"`
	}
	if json.Unmarshal(payload, &event) != nil {
		return
	}
	switch event.Type {
	case "response.completed", "response.incomplete", "response.failed":
		state.markTerminal()
	case "response.reasoning_text.delta", "response.reasoning_summary_text.delta":
		if strings.TrimSpace(event.Delta) != "" {
			state.markGenerated()
			state.hasThinking = true
		}
	case "response.output_item.added", "response.output_item.done":
		noteResponsesReasoningItem(state, event.Item)
		state.aggregateRunes = max(state.aggregateRunes, observeQualityResponsesOutputItem(state, event.Item))
	case "response.output_text.delta":
		if event.Delta != "" {
			noteVisibleContent(state, event.Delta)
		}
	case "response.function_call_arguments.delta", "response.custom_tool_call_input.delta", "response.mcp_call_arguments.delta":
		if event.Delta != "" {
			state.markGenerated()
			state.semanticOutput = true
		}
	}
	if event.Response != nil {
		if state.responseID == "" {
			state.responseID = event.Response.ID
		}
		for _, item := range event.Response.Output {
			noteResponsesReasoningItem(state, item)
		}
		if event.Response.Usage != nil {
			state.usage.Reported = true
			state.usage.InputTokens = event.Response.Usage.InputTokens
			state.usage.OutputTokens = event.Response.Usage.OutputTokens
			state.usage.ReasoningTokens = event.Response.Usage.OutputTokensDetails.ReasoningTokens
			state.usage.TotalTokens = event.Response.Usage.TotalTokens
			state.usage.ResponseModel = event.Response.Model
			state.outputTokens = event.Response.Usage.OutputTokens
			state.reasoningTokens = event.Response.Usage.OutputTokensDetails.ReasoningTokens
		}
		aggregateRunes := 0
		for _, item := range event.Response.Output {
			aggregateRunes += observeQualityResponsesOutputItem(state, item)
		}
		state.aggregateRunes = max(state.aggregateRunes, aggregateRunes)
		// Completed Responses payloads can contain the only copy of visible
		// content for non-delta upstreams. Avoid duplicating it when deltas
		// have already filled the diagnostic preview.
		if state.visibleOutput != nil && state.visibleOutput.sourceBytes == 0 {
			for _, item := range event.Response.Output {
				captureResponsesVisibleOutput(state, item)
			}
		}
	}
}

func captureResponsesVisibleOutput(state *qualityScanState, item qualityResponsesOutputItem) {
	if state == nil || state.visibleOutput == nil || item.Type != "message" {
		return
	}
	for _, content := range item.Content {
		text := content.Text
		if text == "" {
			text = content.Refusal
		}
		state.visibleOutput.appendText(text)
	}
}

func observeQualityResponsesOutputItem(state *qualityScanState, item qualityResponsesOutputItem) int {
	if state == nil {
		return 0
	}
	visibleRunes := 0
	switch item.Type {
	case "", "reasoning":
		return 0
	case "message":
		for _, content := range item.Content {
			text := content.Text
			if text == "" {
				text = content.Refusal
			}
			if text != "" {
				visibleRunes += utf8.RuneCountInString(text)
				state.semanticOutput = true
				continue
			}
			if content.Type != "" && content.Type != "output_text" && content.Type != "refusal" {
				state.semanticOutput = true
			}
		}
	default:
		// Function, shell, MCP and other call items are meaningful output even
		// when the provider omits usage and argument-delta events.
		state.semanticOutput = true
	}
	return visibleRunes
}

func observeQualityAnthropic(state *qualityScanState, payload []byte) {
	var event struct {
		Type         string `json:"type"`
		ContentBlock struct {
			Type string `json:"type"`
			Text string `json:"text"`
			Data string `json:"data"`
		} `json:"content_block"`
		Delta struct {
			Type        string `json:"type"`
			Text        string `json:"text"`
			Thinking    string `json:"thinking"`
			PartialJSON string `json:"partial_json"`
			Signature   string `json:"signature"`
		} `json:"delta"`
		Usage *struct {
			OutputTokens        int64 `json:"output_tokens"`
			OutputTokensDetails struct {
				ThinkingTokens int64 `json:"thinking_tokens"`
			} `json:"output_tokens_details"`
		} `json:"usage"`
	}
	if json.Unmarshal(payload, &event) != nil {
		return
	}
	switch event.Type {
	case "message_stop":
		state.markTerminal()
	case "content_block_start":
		switch event.ContentBlock.Type {
		case "thinking":
			state.markGenerated()
			state.reasoningStarted = true
		case "redacted_thinking":
			state.markGenerated()
			state.reasoningStarted = true
			noteEncryptedBytes(state, event.ContentBlock.Data)
		case "text":
			if event.ContentBlock.Text != "" {
				noteVisibleContent(state, event.ContentBlock.Text)
				state.semanticOutput = true
			}
		case "":
		default:
			state.markGenerated()
			state.semanticOutput = true
		}
	case "content_block_delta":
		if event.Delta.Type == "thinking_delta" && strings.TrimSpace(event.Delta.Thinking) != "" {
			state.markGenerated()
			state.hasThinking = true
		}
		if event.Delta.Type == "signature_delta" && strings.TrimSpace(event.Delta.Signature) != "" {
			// Anthropic Messages represents Responses encrypted_content as a
			// signature delta. Length is judged against the ciphertext floor.
			state.markGenerated()
			state.reasoningStarted = true
			noteEncryptedBytes(state, event.Delta.Signature)
		}
		if event.Delta.Type == "text_delta" && event.Delta.Text != "" {
			noteVisibleContent(state, event.Delta.Text)
		}
		if event.Delta.Type == "input_json_delta" && event.Delta.PartialJSON != "" {
			state.markGenerated()
			state.semanticOutput = true
		}
	}
	if event.Usage != nil {
		state.usage.Reported = true
		state.usage.OutputTokens = event.Usage.OutputTokens
		state.usage.ReasoningTokens = event.Usage.OutputTokensDetails.ThinkingTokens
		state.outputTokens = event.Usage.OutputTokens
		state.reasoningTokens = event.Usage.OutputTokensDetails.ThinkingTokens
	}
}

func noteVisibleContent(state *qualityScanState, text string) {
	if text == "" {
		return
	}
	state.markGenerated()
	if state.firstVisibleAt.IsZero() {
		state.firstVisibleAt = time.Now()
	}
	state.visibleRunes += utf8.RuneCountInString(text)
	if state.visibleOutput != nil {
		state.visibleOutput.appendText(text)
	}
}

func peekQualityStream(ctx context.Context, body io.ReadCloser, protocol string, cfg QualityRetryRuntime) (io.ReadCloser, QualityVerdict, Usage, string, error) {
	result, err := peekQualityStreamCaptured(ctx, body, protocol, cfg)
	return result.replay, result.verdict, result.usage, result.responseID, err
}

type qualityPeekResult struct {
	replay     io.ReadCloser
	verdict    QualityVerdict
	usage      Usage
	responseID string
	capture    qualityStreamCapture
}

func newQualityPeekResult(held *bytes.Buffer, rest io.ReadCloser, state *qualityScanState, verdict QualityVerdict) qualityPeekResult {
	if held == nil {
		held = &bytes.Buffer{}
	}
	if state == nil {
		state = &qualityScanState{}
	}
	return qualityPeekResult{
		replay: newPrefixReplay(held, rest), verdict: verdict, usage: state.usage, responseID: state.responseID,
		capture: newQualityStreamCapture(state.protocol, *state, held.Len(), held.Len() > qualityHoldMaxBufferBytes),
	}
}

func peekQualityStreamCaptured(ctx context.Context, body io.ReadCloser, protocol string, cfg QualityRetryRuntime) (qualityPeekResult, error) {
	cfg = normalizeQualityRetry(cfg)
	state := qualityScanState{
		protocol:                        protocol,
		minEncryptedBytes:               cfg.MinEncryptedBytes,
		encryptedBytesPerReasoningToken: cfg.EncryptedBytesPerReasoningToken,
		startedAt:                       time.Now(),
	}
	if cfg.Trace.Enabled {
		state.visibleOutput = newQualityTextCapture(cfg.Trace.OutputMaxBytes)
	}
	if body == nil {
		return newQualityPeekResult(&bytes.Buffer{}, io.NopCloser(bytes.NewReader(nil)), &state, QualityWait), errQualityEmptyStream
	}
	pump := newQualityReadPump(body)
	var held bytes.Buffer
	holdTimer := time.NewTimer(cfg.HoldTimeout)
	defer holdTimer.Stop()
	for {
		sig := state.signals()
		if !(cfg.MaxOutputTokensPerSecond > 0 && state.terminal && !state.streamEOF) {
			if verdict := classifyQualityHoldWithSpeed(sig, cfg.MinOutputTokens, cfg.MaxOutputTokensPerSecond); verdict != QualityWait {
				return newQualityPeekResult(&held, pump, &state, verdict), nil
			}
		}
		// A completed empty stream must rotate immediately. Waiting for idle
		// timeout after response.completed / [DONE] surfaces HTTP 200 with 0
		// tokens and makes Grok TUI retry for 50–120s.
		if sig.Terminal && (cfg.MaxOutputTokensPerSecond <= 0 || state.streamEOF) {
			return finishQualityPeekCaptured(&held, pump, &state, cfg)
		}

		select {
		case <-ctx.Done():
			_ = pump.Close()
			return newQualityPeekResult(&held, io.NopCloser(bytes.NewReader(nil)), &state, QualityWait), qualityPeekAbortError(ctx, ctx.Err())
		case <-holdTimer.C:
			state.holdExpired = true
			sig.HoldExpired = true
			if !(cfg.MaxOutputTokensPerSecond > 0 && state.terminal && !state.streamEOF) {
				if verdict := classifyQualityHoldWithSpeed(sig, cfg.MinOutputTokens, cfg.MaxOutputTokensPerSecond); verdict != QualityWait {
					return newQualityPeekResult(&held, pump, &state, verdict), nil
				}
			}
		case result, ok := <-pump.results:
			if !ok {
				state.streamEOF = true
				return finishQualityPeekCaptured(&held, pump, &state, cfg)
			}
			if len(result.data) > 0 {
				if held.Len()+len(result.data) > qualityHoldMaxBufferBytes {
					_, _ = held.Write(result.data)
					ObserveQualityChunk(&state, result.data)
					state.holdExpired = true
					verdict := classifyQualityHoldWithSpeed(state.signals(), cfg.MinOutputTokens, cfg.MaxOutputTokensPerSecond)
					if verdict == QualityWait {
						verdict = QualityWithhold
					}
					return newQualityPeekResult(&held, pump, &state, verdict), nil
				}
				_, _ = held.Write(result.data)
				ObserveQualityChunk(&state, result.data)
			}
			if result.err == io.EOF {
				state.streamEOF = true
				return finishQualityPeekCaptured(&held, pump, &state, cfg)
			}
			if result.err != nil {
				_ = pump.Close()
				return newQualityPeekResult(&held, io.NopCloser(bytes.NewReader(nil)), &state, QualityWait), qualityPeekAbortError(ctx, result.err)
			}
		}
	}
}

func finishQualityPeekCaptured(held *bytes.Buffer, pump *qualityReadPump, state *qualityScanState, cfg QualityRetryRuntime) (qualityPeekResult, error) {
	if state == nil {
		return newQualityPeekResult(held, pump, nil, QualityWait), errQualityEmptyStream
	}
	if len(state.pending) > 0 {
		// Process a final valid SSE data line even when the upstream omitted its
		// trailing newline.
		ObserveQualityChunk(state, []byte{'\n'})
	}
	state.markTerminal()
	signals := state.signals()
	if !signals.HasThinking && signals.ReasoningTokens <= 0 && signals.OutputTokens <= 0 && signals.VisibleTokens <= 0 {
		if state.semanticOutput {
			return newQualityPeekResult(held, pump, state, QualityDeliver), nil
		}
		return newQualityPeekResult(held, pump, state, QualityWait), errQualityEmptyStream
	}
	return newQualityPeekResult(held, pump, state, classifyQualityHoldWithSpeed(signals, cfg.MinOutputTokens, cfg.MaxOutputTokensPerSecond)), nil
}

func newPrefixReplay(held *bytes.Buffer, rest io.ReadCloser) io.ReadCloser {
	if rest == nil {
		rest = io.NopCloser(bytes.NewReader(nil))
	}
	if held == nil || held.Len() == 0 {
		return rest
	}
	return &replayReadCloser{Reader: io.MultiReader(bytes.NewReader(held.Bytes()), rest), source: rest}
}
