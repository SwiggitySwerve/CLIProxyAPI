package auth

import (
	"context"
	"testing"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

// Characterization tests for the provider websocket transport preference
// (roadmap 3.2 group 6). They pin today's per-provider behaviour so moving the
// fact into the provider registry cannot change it.

func TestProviderTransportCharacterization_PrefersWebsocket(t *testing.T) {
	for _, tc := range []struct {
		provider string
		want     bool
	}{
		{"codex", true},
		{" Codex ", true},
		{"xai", true},
		{"XAI", true},
		// Login and catalog aliases are not scheduling providers.
		{"openai", false},
		{"x-ai", false},
		{"x.ai", false},
		{"grok", false},
		{"claude", false},
		{"gemini", false},
		{"gemini-interactions", false},
		{"interactions", false},
		{"vertex", false},
		{"aistudio", false},
		{"antigravity", false},
		{"devin", false},
		{"meta", false},
		{"kimi", false},
		{"kimi-ai", false},
		{"openai-compatibility", false},
		{"openai-compatible-example", false},
		{"mixed", false},
		{"unknown", false},
		{"", false},
	} {
		if got := providerPrefersWebsocketTransport(tc.provider); got != tc.want {
			t.Errorf("providerPrefersWebsocketTransport(%q) = %t, want %t", tc.provider, got, tc.want)
		}
	}
}

// A provider without the preference keeps rotating over every ready credential
// on a downstream websocket, websocket-enabled or not.
func TestProviderTransportCharacterization_NoPreferenceKeepsFullPool(t *testing.T) {
	scheduler := newSchedulerForTest(
		&RoundRobinSelector{},
		&Auth{ID: "claude-http", Provider: "claude"},
		&Auth{ID: "claude-ws", Provider: "claude", Attributes: map[string]string{"websockets": "true"}},
	)
	ctx := cliproxyexecutor.WithDownstreamWebsocket(context.Background())
	for index, wantID := range []string{"claude-http", "claude-ws", "claude-http"} {
		got, errPick := scheduler.pickSingle(ctx, "claude", "", cliproxyexecutor.Options{}, nil)
		if errPick != nil || got == nil {
			t.Fatalf("pickSingle() #%d: auth=%v err=%v", index, got, errPick)
		}
		if got.ID != wantID {
			t.Fatalf("pickSingle() #%d auth.ID = %q, want %q", index, got.ID, wantID)
		}
	}
}
