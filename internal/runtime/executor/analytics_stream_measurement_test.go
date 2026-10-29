package executor

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/clienterror"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

// analyticsCancelBody cancels only after the fixture's usage frames are read.
// It exercises the scanner error path without a timer or real upstream connection.
type analyticsCancelBody struct {
	reader io.Reader
	cancel context.CancelFunc
}

func (b *analyticsCancelBody) Read(p []byte) (int, error) {
	n, err := b.reader.Read(p)
	if err == io.EOF {
		b.cancel()
		return n, context.Canceled
	}
	return n, err
}
func (*analyticsCancelBody) Close() error { return nil }

func TestAnalyticsProviderStreamMeasurement(t *testing.T) {
	for _, route := range []string{"claude-native", "claude-translated", "kimi-chat", "kimi-responses"} {
		for _, scenario := range []string{"no-usage", "explicit-zero", "cancel-no-usage", "cancel-partial"} {
			t.Run(route+"/"+scenario, func(t *testing.T) {
				canceled := strings.HasPrefix(scenario, "cancel")
				reported := scenario == "explicit-zero" || scenario == "cancel-partial"
				input := "0"
				if scenario == "cancel-partial" {
					input = "3"
				}
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				alias := t.Name()
				capture := &multiProviderUsageCapture{alias: alias, records: make(chan usage.Record, 8)}
				usage.RegisterNamedPlugin(t.Name(), capture)
				defer usage.RegisterNamedPlugin(t.Name(), multiProviderNoopUsagePlugin{})
				var sse string
				source, response := sdktranslator.FormatClaude, sdktranslator.FormatClaude
				payload := []byte(`{"model":"claude-opus-5","messages":[{"role":"user","content":"hi"}],"stream":true}`)
				model := "claude-opus-5"
				switch route {
				case "claude-native", "claude-translated":
					u := ""
					if reported {
						u = `,"usage":{"input_tokens":` + input + `,"output_tokens":0}`
					}
					sse = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_test\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"claude-opus-5\",\"content\":[]" + u + "}}\n\n"
					if !canceled {
						sse += "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
					}
					if route == "claude-translated" {
						response = sdktranslator.FormatOpenAIResponse
					}
				case "kimi-chat":
					source, response = sdktranslator.FormatOpenAI, sdktranslator.FormatOpenAI
					model = "kimi-k3"
					sse = "data: {\"id\":\"chat_test\",\"model\":\"k3\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hello\"}}]}\n\n"
					if reported {
						sse += `data: {"choices":[],"usage":{"prompt_tokens":` + input + `,"completion_tokens":0}}` + "\n\n"
					}
					if !canceled {
						sse += "data: [DONE]\n\n"
					}
				case "kimi-responses":
					source, response = sdktranslator.FormatOpenAIResponse, sdktranslator.FormatOpenAIResponse
					model = "kimi-k3"
					payload = []byte(`{"model":"kimi-k3","input":"hi","stream":true}`)
					sse = "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_test\",\"model\":\"k3\",\"status\":\"in_progress\"}}\n\n"
					if !canceled || reported {
						u := ""
						if reported {
							u = `,"usage":{"input_tokens":` + input + `,"output_tokens":0}`
						}
						event := "response.completed"
						if canceled {
							event = "response.incomplete"
						}
						sse += "event: " + event + "\ndata: {\"type\":\"" + event + "\",\"response\":{\"id\":\"resp_test\",\"output\":[]" + u + "}}\n\n"
					}
				}
				ctx = usage.WithRequestedModelAlias(ctx, alias)
				ctx = usage.WithStream(ctx, true)
				ctx = context.WithValue(ctx, "cliproxy.roundtripper", kimiRoundTripperFunc(func(req *http.Request) (*http.Response, error) {
					var body io.ReadCloser = io.NopCloser(strings.NewReader(sse))
					if canceled {
						body = &analyticsCancelBody{reader: strings.NewReader(sse), cancel: cancel}
					}
					return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: body, Request: req}, nil
				}))
				auth := &cliproxyauth.Auth{Attributes: map[string]string{"api_key": "fixture", "base_url": "http://fixture.invalid"}}
				var exec interface {
					ExecuteStream(context.Context, *cliproxyauth.Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error)
				}
				if strings.HasPrefix(route, "claude") {
					exec = NewClaudeExecutor(&config.Config{})
				} else {
					exec = NewKimiExecutor(&config.Config{})
				}
				result, err := exec.ExecuteStream(ctx, auth, cliproxyexecutor.Request{Model: model, Payload: payload}, cliproxyexecutor.Options{SourceFormat: source, ResponseFormat: response, Stream: true})
				if err != nil {
					t.Fatal(err)
				}
				for chunk := range result.Chunks {
					if chunk.Err != nil && !canceled {
						t.Fatal(chunk.Err)
					}
				}
				// Channel closure follows deferred publication; this fence observes all records.
				usage.PublishRecord(context.Background(), usage.Record{RequestID: "analytics-fence", Alias: alias})
				count := 0
				for {
					rec := capture.await(t)
					if rec.RequestID == "analytics-fence" {
						break
					}
					count++
					if rec.Failed != canceled || rec.Detail.UsageFields.Reported() != reported {
						t.Fatalf("failed=%v usage=%s", rec.Failed, rec.Detail.UsageFields.Presence())
					}
					if canceled && rec.Fail.StatusCode != clienterror.StatusClientClosedRequest {
						t.Fatalf("cancel status=%d", rec.Fail.StatusCode)
					}
					if scenario == "cancel-partial" && rec.Detail.InputTokens != 3 {
						t.Fatalf("partial input=%d", rec.Detail.InputTokens)
					}
					if rec.RecordKind != usage.RecordExecution || rec.TTFTMethod != usage.TimingFirstByte || rec.Timing == nil || rec.Timing.FirstResponseMs == nil || rec.Timing.FirstContentMs != nil {
						t.Fatal("provider transport provenance lost")
					}
				}
				if count != 1 {
					t.Fatalf("usage record count=%d, want exactly 1", count)
				}
			})
		}
	}
}
