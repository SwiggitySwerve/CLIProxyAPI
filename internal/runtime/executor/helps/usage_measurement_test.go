package helps

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/clienterror"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

func TestUsageMeasurementPresence(t *testing.T) {
	for _, tc := range []struct {
		name, payload           string
		parse                   func([]byte) usage.Detail
		reported, input, output bool
	}{
		{"openai absent", `{}`, ParseOpenAIUsage, false, false, false},
		{"openai null", `{"usage":null}`, ParseOpenAIUsage, false, false, false},
		{"openai invalid fields", `{"usage":{"input_tokens":null,"output_tokens":"0"}}`, ParseOpenAIUsage, false, false, false},
		{"openai explicit zero", `{"usage":{"input_tokens":0,"output_tokens":0}}`, ParseOpenAIUsage, true, true, true},
		{"openai tier only", `{"service_tier":"priority"}`, ParseOpenAIUsage, false, false, false},
		{"openai partial", `{"usage":{"output_tokens":0}}`, ParseOpenAIUsage, true, false, true},
		{"claude absent", `{}`, ParseClaudeUsage, false, false, false},
		{"claude empty", `{"usage":{}}`, ParseClaudeUsage, false, false, false},
		{"claude explicit zero", `{"usage":{"input_tokens":0,"output_tokens":0}}`, ParseClaudeUsage, true, true, true},
		{"claude partial", `{"usage":{"output_tokens":0}}`, ParseClaudeUsage, true, false, true},
		{"codex explicit zero", `{"response":{"usage":{"input_tokens":0,"output_tokens":0}}}`, func(b []byte) usage.Detail { d, _ := ParseCodexUsage(b); return d }, true, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := tc.parse([]byte(tc.payload))
			if d.UsageFields.Reported() != tc.reported {
				t.Fatalf("presence = %+v", d.UsageFields)
			}
			if tc.reported && (d.UsageFields.InputTokens != tc.input || d.UsageFields.OutputTokens != tc.output) {
				t.Fatalf("fields = %+v", d.UsageFields)
			}
		})
	}
	// Normalizing zero must not manufacture evidence of measured usage.
	d := normalizeUsageDetailTotal(ParseOpenAIUsage([]byte(`{}`)), "openai", "")
	if d.UsageFields.Reported() {
		t.Fatal("normalization invented presence")
	}
}

func TestUsageMeasurementStreamMerge(t *testing.T) {
	var b StreamUsageBuffer
	b.ObserveClaudeStream([]byte(`data: {"message":{"usage":{"input_tokens":0,"cache_read_input_tokens":0}}}`))
	b.ObserveClaudeStream([]byte(`data: {"usage":{"output_tokens":0}}`))
	d, ok := b.Detail()
	if !ok || d.UsageFields == nil || !d.UsageFields.InputTokens || !d.UsageFields.OutputTokens || !d.UsageFields.CacheReadTokens || d.UsageFields.TotalTokens {
		t.Fatalf("fields = %+v", d.UsageFields)
	}
	var openai StreamUsageBuffer
	openai.ObserveOpenAIStream([]byte(`data: {"service_tier":"priority"}`))
	if openai.detail.UsageFields.Reported() {
		t.Fatal("tier is not token usage")
	}
	openai.ObserveOpenAIStream([]byte(`data: {"usage":{"input_tokens":0,"output_tokens":0},"service_tier":"priority"}`))
	if !openai.detail.UsageFields.Reported() {
		t.Fatal("explicit zero with tier was discarded")
	}
	openai.ObserveOpenAIStream([]byte(`data: {"service_tier":"default"}`))
	if !openai.detail.UsageFields.Reported() {
		t.Fatal("tier update erased usage")
	}
}

func TestUsageReporterMeasurementTimingTransport(t *testing.T) {
	for _, tc := range []struct {
		name, body          string
		packetOnly, content bool
		method              usage.TimingMethod
	}{
		{"first byte", "metadata", false, false, usage.TimingFirstByte},
		{"fallback packet", "metadata", true, false, usage.TimingFallbackPacket},
		{"substantive content", "metadata then text", true, true, usage.TimingSubstantiveContent},
		{"unavailable", "", true, false, usage.TimingUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := NewUsageReporter(context.Background(), "test", "model", nil)
			// Give execution preparation a deterministic distinct origin without sleeping.
			r.requestedAt = r.requestedAt.Add(-time.Second)
			client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(tc.body)), Header: make(http.Header), Request: req}, nil
			})}
			if tc.packetOnly {
				client = r.TrackHTTPClientRoundTripOnly(client)
			} else {
				client = r.TrackHTTPClient(client)
			}
			req, _ := http.NewRequest(http.MethodGet, "http://provider.invalid", nil)
			resp, err := client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := io.ReadAll(resp.Body); err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if tc.content {
				r.ObserveTokenEvent(true)
			}
			rec := r.buildRecord(usage.Detail{}, false)
			if rec.TTFTMethod != tc.method {
				t.Fatalf("method = %q", rec.TTFTMethod)
			}
			m := rec.Timing
			if m == nil || m.Origin != "execution-start" || m.UpstreamStartMs == nil || *m.UpstreamStartMs < 1000 || m.RecordedMs != rec.Latency.Milliseconds() {
				t.Fatalf("timing = %+v", m)
			}
			if (m.FirstResponseMs != nil) != (tc.body != "") || (m.FirstContentMs != nil) != tc.content {
				t.Fatalf("measurement presence = %+v", m)
			}
			if m.FirstResponseMs != nil && (*m.FirstResponseMs < *m.UpstreamStartMs || *m.FirstResponseMs > m.RecordedMs) {
				t.Fatal("response outside common-origin interval")
			}
			if tc.content && (*m.FirstContentMs < *m.FirstResponseMs || *m.FirstContentMs > m.RecordedMs) {
				t.Fatal("content outside common-origin interval")
			}
			if tc.method != usage.TimingUnavailable {
				first := m.FirstResponseMs
				if tc.content {
					first = m.FirstContentMs
				}
				delta := *first - *m.UpstreamStartMs - rec.TTFT.Milliseconds()
				if delta < -1 || delta > 1 {
					t.Fatalf("legacy TTFT and offsets diverge: %d ms", delta)
				}
			}
		})
	}
}

type measurementCapture chan usage.Record

func (c measurementCapture) HandleUsage(_ context.Context, r usage.Record) { c <- r }

func TestStreamUsageCompleteExactlyOnce(t *testing.T) {
	for _, tc := range []struct {
		name                             string
		partial, cancel, completed, fail bool
	}{
		{name: "no usage"},
		{name: "partial", partial: true},
		{name: "cancel", cancel: true, fail: true},
		{name: "partial cancel", partial: true, cancel: true, fail: true},
		{name: "terminal then cancel", cancel: true, completed: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			records := make(measurementCapture, 8)
			usage.RegisterNamedPlugin("measurement-complete", records)
			defer usage.RegisterNamedPlugin("measurement-complete", measurementNoop{})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			r := NewUsageReporter(ctx, "test", "model", nil)
			var b StreamUsageBuffer
			if tc.partial {
				b.ObserveOpenAIStream([]byte(`data: {"usage":{"input_tokens":3,"output_tokens":0}}`))
			}
			if tc.cancel {
				cancel()
			}
			b.Complete(ctx, r, tc.completed)
			b.Complete(ctx, r, tc.completed)
			r.EnsurePublished(ctx)
			usage.PublishRecord(ctx, usage.Record{RequestID: "measurement-barrier"})
			count := 0
			for {
				select {
				case rec := <-records:
					if rec.RequestID == "measurement-barrier" {
						if count != 1 {
							t.Fatalf("records = %d, want 1", count)
						}
						usage.RegisterNamedPlugin("measurement-complete", measurementNoop{})
						return
					}
					if rec.RequestID != r.RequestID() {
						continue
					}
					count++
					if rec.Failed != tc.fail || rec.Detail.UsageFields.Reported() != tc.partial {
						t.Fatalf("failed=%v presence=%s", rec.Failed, rec.Detail.UsageFields.Presence())
					}
					if tc.partial && rec.Detail.InputTokens != 3 {
						t.Fatal("partial tokens lost")
					}
					if tc.fail && rec.Fail.StatusCode != clienterror.StatusClientClosedRequest {
						t.Fatalf("cancel status = %d", rec.Fail.StatusCode)
					}
				case <-time.After(3 * time.Second):
					t.Fatal("usage dispatch timed out")
				}
			}
		})
	}
}

type measurementNoop struct{}

func (measurementNoop) HandleUsage(context.Context, usage.Record) {}

func TestSupplementaryUsageMeasurementRelation(t *testing.T) {
	r := NewUsageReporter(context.Background(), "openai", "parent-model", nil)
	detail := ParseOpenAIUsage([]byte(`{"usage":{"input_tokens":3,"output_tokens":2}}`))
	rec, ok := r.buildAdditionalModelRecord("image-model", detail)
	if !ok || rec.RecordKind != usage.RecordSupplementary || rec.ParentExecutionID != r.RequestID() || rec.RequestID == r.RequestID() || !rec.Detail.UsageFields.Reported() {
		t.Fatalf("invalid supplementary relationship: kind=%q parent=%q", rec.RecordKind, rec.ParentExecutionID)
	}
	parent := r.buildRecord(detail, false)
	if parent.RecordKind != usage.RecordExecution || parent.ParentExecutionID != "" {
		t.Fatal("regular execution relation changed")
	}
}
