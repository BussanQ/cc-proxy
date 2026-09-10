package executor

import (
	"testing"

	cursorauth "github.com/router-for-me/CLIProxyAPI/v7/internal/auth/cursor"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func TestResolveCursorModelEffortVariant(t *testing.T) {
	auth := &cliproxyauth.Auth{Metadata: map[string]any{cursorauth.ModelCacheKey: []cursorauth.ModelDetails{
		{ID: "gpt-test"}, {ID: "gpt-test-low"}, {ID: "gpt-test-high"}, {ID: "gpt-test-high-fast"}, {ID: "gpt-test-medium-fast"},
	}}}
	model, err := resolveCursorModel(auth, "gpt-test-low", "high")
	if err != nil {
		t.Fatalf("resolveCursorModel() error = %v", err)
	}
	if model != "gpt-test-high" {
		t.Fatalf("model = %q", model)
	}
	if _, err = resolveCursorModel(auth, "gpt-test", "xhigh"); err == nil {
		t.Fatal("missing effort variant was accepted")
	}
	model, err = resolveCursorModel(auth, "gpt-test-high-fast", "medium")
	if err != nil {
		t.Fatalf("resolveCursorModel() fast variant error = %v", err)
	}
	if model != "gpt-test-medium-fast" {
		t.Fatalf("fast model = %q", model)
	}
}

func TestResolveCursorModelRemovesLegacyPrefix(t *testing.T) {
	auth := &cliproxyauth.Auth{Metadata: map[string]any{cursorauth.ModelCacheKey: []cursorauth.ModelDetails{
		{ID: "cursor-grok-4.6-high-fast"},
	}}}
	for _, requested := range []string{"grok-4.6-high-fast", "cursor-grok-4.6-high-fast"} {
		model, err := resolveCursorModel(auth, requested, "")
		if err != nil {
			t.Fatalf("resolveCursorModel(%q) error = %v", requested, err)
		}
		if model != "grok-4.6-high-fast" {
			t.Fatalf("resolveCursorModel(%q) = %q", requested, model)
		}
		if upstream := cursorUpstreamModelID(auth, model); upstream != "cursor-grok-4.6-high-fast" {
			t.Fatalf("cursorUpstreamModelID(%q) = %q", model, upstream)
		}
	}
}

func TestCursorPublicResponseTextRemovesCatalogPrefix(t *testing.T) {
	got := cursorPublicResponseText(`> The model "cursor-grok-4.6-high-fast" is unavailable and you have been rerouted to Auto.`)
	want := `> The model "grok-4.6-high-fast" is unavailable and you have been rerouted to Auto.`
	if got != want {
		t.Fatalf("cursorPublicResponseText() = %q, want %q", got, want)
	}
}

func TestResolveCursorModelAliases(t *testing.T) {
	auth := &cliproxyauth.Auth{Metadata: map[string]any{cursorauth.ModelCacheKey: []cursorauth.ModelDetails{
		{ID: "gemini-test-high", Aliases: []string{"gemini-test", "latest", "canonical"}},
		{ID: "gemini-test-low", Aliases: []string{"latest"}},
		{ID: "canonical"},
	}}}
	for _, tc := range []struct{ requested, effort, want string }{
		{"gemini-test", "", "gemini-test-high"},
		{"cursor-gemini-test", "auto", "gemini-test-high"},
		{"gemini-test", "low", "gemini-test-low"},
		{"latest", "", "gemini-test-high"},
		{"canonical", "", "canonical"},
	} {
		got, err := resolveCursorModel(auth, tc.requested, tc.effort)
		if err != nil || got != tc.want {
			t.Fatalf("resolve(%q,%q)=%q,%v want %q", tc.requested, tc.effort, got, err, tc.want)
		}
	}
	for _, tc := range []struct{ model, effort string }{{"missing", ""}, {"gemini-test", "max"}} {
		_, err := resolveCursorModel(auth, tc.model, tc.effort)
		scoped, ok := err.(interface{ IsRequestScoped() bool })
		if !ok || !scoped.IsRequestScoped() {
			t.Fatalf("invalid model/effort must be request-scoped: %v", err)
		}
	}
}
