package usage

// TokenUsageFields records numeric fields observed upstream, independently of
// normalization and accounting quality. A nil pointer means no observation.
type TokenUsageFields struct {
	InputTokens         bool `json:"input_tokens"`
	OutputTokens        bool `json:"output_tokens"`
	ReasoningTokens     bool `json:"reasoning_tokens"`
	CacheReadTokens     bool `json:"cache_read_tokens"`
	CacheCreationTokens bool `json:"cache_creation_tokens"`
	TotalTokens         bool `json:"total_tokens"`
}

func (f *TokenUsageFields) Reported() bool {
	return f != nil && (f.InputTokens || f.OutputTokens || f.ReasoningTokens ||
		f.CacheReadTokens || f.CacheCreationTokens || f.TotalTokens)
}

func (f *TokenUsageFields) Presence() string {
	if f.Reported() {
		return "reported"
	}
	return "unknown"
}

// MergeTokenUsageFields combines observations without mutating published values.
func MergeTokenUsageFields(a, b *TokenUsageFields) *TokenUsageFields {
	if a == nil && b == nil {
		return nil
	}
	var merged TokenUsageFields
	if a != nil {
		merged = *a
	}
	if b != nil {
		merged.InputTokens = merged.InputTokens || b.InputTokens
		merged.OutputTokens = merged.OutputTokens || b.OutputTokens
		merged.ReasoningTokens = merged.ReasoningTokens || b.ReasoningTokens
		merged.CacheReadTokens = merged.CacheReadTokens || b.CacheReadTokens
		merged.CacheCreationTokens = merged.CacheCreationTokens || b.CacheCreationTokens
		merged.TotalTokens = merged.TotalTokens || b.TotalTokens
	}
	return &merged
}

type TimingMethod string

const (
	TimingSubstantiveContent TimingMethod = "substantive-content"
	TimingFirstByte          TimingMethod = "first-byte"
	TimingFallbackPacket     TimingMethod = "fallback-packet"
	TimingUnavailable        TimingMethod = "unavailable"
)

// ExecutionTiming offsets all use the reporter's execution-start monotonic clock.
// RecordedMs is a snapshot time, not a client delivery or completion guarantee.
type ExecutionTiming struct {
	Origin          string `json:"origin"`
	UpstreamStartMs *int64 `json:"upstream_start_ms"`
	FirstResponseMs *int64 `json:"first_response_ms"`
	FirstContentMs  *int64 `json:"first_content_ms"`
	RecordedMs      int64  `json:"recorded_ms"`
}

type RecordKind string

const (
	RecordExecution     RecordKind = "execution"
	RecordSupplementary RecordKind = "supplementary"
	RecordUnknown       RecordKind = "unknown"
)
