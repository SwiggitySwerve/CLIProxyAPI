package executor

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

// Upstream request compression must not let a compressed body hide an ultrafast
// tier from the account gate: the guard reads the plaintext JSON, before any send.
func TestUltrafastGuardRejectsCompressedRequestBeforeUpstream(t *testing.T) {
	for _, mode := range []string{"execute", "stream", "compact"} {
		t.Run(mode, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.WriteHeader(http.StatusInternalServerError)
			}))
			defer server.Close()
			localURL, errURL := url.Parse(server.URL)
			if errURL != nil {
				t.Fatal(errURL)
			}
			transport := roundTripperFunc(func(req *http.Request) (*http.Response, error) {
				mapped := req.Clone(req.Context())
				mapped.URL.Scheme, mapped.URL.Host = localURL.Scheme, localURL.Host
				mapped.Host = localURL.Host
				return http.DefaultTransport.RoundTrip(mapped)
			})
			ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", transport)
			credential := codexCompressionAuth()
			credential.ID = "other-account"
			credential.Metadata["email"] = "someone-else@example.com"
			payload := []byte(`{"model":"gpt-5.4","service_tier":"ultrafast","input":[{"role":"user","content":"` + strings.Repeat("compressible input ", 400) + `"}]}`)
			req := cliproxyexecutor.Request{Model: "gpt-5.4", Payload: payload}
			opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse}
			executor := NewCodexExecutor(codexCompressionConfig())
			var err error
			switch mode {
			case "execute":
				_, err = executor.Execute(ctx, credential, req, opts)
			case "stream":
				_, err = executor.ExecuteStream(ctx, credential, req, opts)
			case "compact":
				opts.Alt = "responses/compact"
				_, err = executor.Execute(ctx, credential, req, opts)
			}
			if err == nil {
				t.Fatal("expected ultrafast account rejection")
			}
			if calls.Load() != 0 {
				t.Fatalf("disallowed account reached upstream %d times", calls.Load())
			}
		})
	}
}
