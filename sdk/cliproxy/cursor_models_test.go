package cliproxy

import (
	"testing"

	cursorauth "github.com/router-for-me/CLIProxyAPI/v7/internal/auth/cursor"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func TestBuildCursorCachedModelsThinkingVariants(t *testing.T) {
	auth := &coreauth.Auth{Metadata: map[string]any{cursorauth.ModelCacheKey: []any{
		map[string]any{"id": "cursor-gpt-test", "display_name": "GPT Test"},
		map[string]any{"id": "cursor-gpt-test-low", "thinking": true},
		map[string]any{"id": "cursor-gpt-test-high", "thinking": true},
	}}}
	models := buildCursorCachedModels(auth)
	if len(models) != 3 {
		t.Fatalf("models = %d, want 3", len(models))
	}
	if models[0].ID != "gpt-test" || models[0].Thinking == nil {
		t.Fatalf("unexpected base model: %#v", models[0])
	}
	want := []string{"none", "low", "high"}
	if len(models[0].Thinking.Levels) != len(want) {
		t.Fatalf("levels = %v", models[0].Thinking.Levels)
	}
	for i := range want {
		if models[0].Thinking.Levels[i] != want[i] {
			t.Fatalf("levels = %v", models[0].Thinking.Levels)
		}
	}
}

func TestBuildCursorCachedModelsAliasesAndExclusions(t *testing.T) {
	auth := &coreauth.Auth{Metadata: map[string]any{cursorauth.ModelCacheKey: []cursorauth.ModelDetails{
		{ID: "gemini-test-high", Aliases: []string{"gemini-test", "latest", "canonical"}},
		{ID: "gemini-test-low", Aliases: []string{"latest", "low-alias"}},
		{ID: "canonical"},
	}}}
	for _, tc := range []struct {
		name           string
		excluded, want []string
	}{
		{"all", nil, []string{"gemini-test-high", "gemini-test-low", "canonical", "gemini-test", "latest", "low-alias"}},
		{"exclude canonical", []string{"gemini-test-high"}, []string{"gemini-test-low", "canonical", "low-alias"}},
		{"exclude alias", []string{"latest"}, []string{"gemini-test-high", "gemini-test-low", "canonical", "gemini-test", "low-alias"}},
		{"wildcard", []string{"gemini-*"}, []string{"canonical"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			models := buildCursorCachedModels(auth, tc.excluded...)
			if len(models) != len(tc.want) {
				t.Fatalf("got %d models want %v", len(models), tc.want)
			}
			for i, m := range models {
				if m.ID != tc.want[i] {
					t.Fatalf("model %d=%q want %q", i, m.ID, tc.want[i])
				}
			}
			if tc.name == "all" {
				alias := models[3]
				if alias.Thinking == nil || alias.ContextLength != models[0].ContextLength || len(alias.Thinking.Levels) != 2 {
					t.Fatalf("alias capabilities lost: %+v", alias)
				}
			}
		})
	}
}
