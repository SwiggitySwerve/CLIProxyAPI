package redisqueue

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	coreusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

func TestUsageQueueMeasurementContract(t *testing.T) {
	for _, reported := range []bool{false, true} {
		t.Run(map[bool]string{false: "unknown", true: "explicit zero"}[reported], func(t *testing.T) {
			withEnabledQueue(t, func() {
				zero := int64(0)
				record := coreusage.Record{
					RequestID: "execution-child", TraceID: "parent-trace",
					RecordKind: coreusage.RecordSupplementary, ParentExecutionID: "execution-parent",
					Provider: "openai", Model: "test", RequestedAt: time.Unix(100, 0),
					Latency: 5 * time.Millisecond, TTFTMethod: coreusage.TimingFirstByte,
					Timing: &coreusage.ExecutionTiming{Origin: "execution-start", UpstreamStartMs: &zero, FirstResponseMs: &zero, RecordedMs: 5},
				}
				if reported {
					record.Detail.UsageFields = &coreusage.TokenUsageFields{InputTokens: true, OutputTokens: true, TotalTokens: true}
				}
				(&usageQueuePlugin{instanceID: "codex-primary"}).HandleUsage(context.Background(), record)
				payload := popSinglePayload(t)
				requireStringField(t, payload, "instance_id", "codex-primary")
				requireStringField(t, payload, "usage_presence", map[bool]string{false: "unknown", true: "reported"}[reported])
				requireStringField(t, payload, "ttft_method", "first-byte")
				requireStringField(t, payload, "record_kind", "supplementary")
				requireStringField(t, payload, "parent_execution_id", "execution-parent")
				requireStringField(t, payload, "execution_id", "execution-child")
				requireStringField(t, payload, "trace_id", "parent-trace")
				var timing map[string]json.RawMessage
				if err := json.Unmarshal(payload["timing"], &timing); err != nil {
					t.Fatal(err)
				}
				if string(timing["first_content_ms"]) != "null" || string(timing["first_response_ms"]) != "0" {
					t.Fatalf("missing and zero timing conflated: %s", payload["timing"])
				}
				if (string(payload["usage_fields"]) != "null") != reported {
					t.Fatalf("presence fields = %s", payload["usage_fields"])
				}
				requireIntField(t, payload, "latency_ms", 5)
				requireIntField(t, payload, "ttft_ms", 0)
				requireTokenBreakdown(t, payload, coreusage.TokenAccountingQualityComplete, 0)
			})
		})
	}
	withEnabledQueue(t, func() {
		(&usageQueuePlugin{}).HandleUsage(context.Background(), coreusage.Record{Provider: "openai", Model: "legacy", Detail: coreusage.Detail{InputTokens: 5}})
		payload := popSinglePayload(t)
		requireStringField(t, payload, "usage_presence", "unknown")
		requireStringField(t, payload, "record_kind", "unknown")
		requireStringField(t, payload, "ttft_method", "unavailable")
		if string(payload["timing"]) != "null" {
			t.Fatal("legacy record acquired fabricated timing")
		}
	})
}

func TestDeploymentInstanceID(t *testing.T) {
	for input, want := range map[string]string{
		"codex-primary": "codex-primary", " claude-primary ": "claude-primary",
		"": "", "https://secret.test": "", "User@Example.test": "", "Uppercase": "", "bad\nvalue": "",
	} {
		if got := deploymentInstanceID(input); got != want {
			t.Errorf("deploymentInstanceID(%q)=%q, want %q", input, got, want)
		}
	}
}
