package executor

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	cursorauth "github.com/router-for-me/CLIProxyAPI/v7/internal/auth/cursor"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	cp "github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps/cursorproto"
	_ "github.com/router-for-me/CLIProxyAPI/v7/internal/translator"
	auth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	x "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	tr "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
	"google.golang.org/protobuf/proto"
)

func cursorExecutionTestAuth(id string) *auth.Auth {
	return &auth.Auth{ID: id, Provider: "cursor", Status: auth.StatusActive, Metadata: map[string]any{
		"access_token":           "test-token",
		cursorauth.ModelCacheKey: []cursorauth.ModelDetails{{ID: "cursor-test-model-high", Aliases: []string{"cursor-test-alias"}}},
	}}
}

func cursorExecutionTestFrame(flag byte, payload []byte) []byte {
	frame := make([]byte, 5+len(payload))
	frame[0] = flag
	binary.BigEndian.PutUint32(frame[1:5], uint32(len(payload)))
	copy(frame[5:], payload)
	return frame
}

func cursorExecutionTestContext(t *testing.T, respond func(*cp.AgentRunRequest) *http.Response) context.Context {
	t.Helper()
	transport := roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		defer req.Body.Close()
		var header [5]byte
		if _, err := io.ReadFull(req.Body, header[:]); err != nil {
			return nil, err
		}
		payload := make([]byte, binary.BigEndian.Uint32(header[1:]))
		if _, err := io.ReadFull(req.Body, payload); err != nil {
			return nil, err
		}
		var msg cp.AgentClientMessage
		if err := proto.Unmarshal(payload, &msg); err != nil {
			return nil, err
		}
		return respond(msg.GetRunRequest()), nil
	})
	return context.WithValue(context.Background(), "cliproxy.roundtripper", transport)
}

func TestCursorExecutorStreamsMultipleToolsThroughAlias(t *testing.T) {
	for _, format := range []tr.Format{tr.FormatOpenAI, tr.FormatClaude} {
		t.Run(format.String(), func(t *testing.T) {
			ctx := cursorExecutionTestContext(t, func(run *cp.AgentRunRequest) *http.Response {
				// NormalizeModelID removes the catalog's cursor- prefix.
				if run.RequestedModel.ModelId != "cursor-test-model-high" {
					t.Errorf("upstream model=%q", run.RequestedModel.ModelId)
				}
				var frames []byte
				for _, id := range []string{"call_1", "call_2"} {
					payload, err := proto.Marshal(&cp.AgentServerMessage{Message: &cp.AgentServerMessage_ExecServerMessage{ExecServerMessage: &cp.ExecServerMessage{Message: &cp.ExecServerMessage_McpArgs{McpArgs: &cp.McpArgs{Name: "mcp__cliproxy__Read", ToolCallId: id}}}}})
					if err != nil {
						t.Fatal(err)
					}
					frames = append(frames, cursorExecutionTestFrame(0, payload)...)
				}
				frames = append(frames, cursorExecutionTestFrame(2, []byte(`{}`))...)
				return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(frames))}
			})
			payload := []byte(`{"stream":true,"model":"test-alias","messages":[{"role":"user","content":"read both"}],"tools":[{"type":"function","function":{"name":"Read","parameters":{"type":"object"}}}]}`)
			stream, err := NewCursorExecutor(nil).ExecuteStream(ctx, cursorExecutionTestAuth("parallel-test"), x.Request{Model: "test-alias", Payload: payload}, x.Options{Stream: true, SourceFormat: tr.FormatOpenAI, ResponseFormat: format, OriginalRequest: payload})
			if err != nil {
				t.Fatal(err)
			}
			calls := 0
			finish := ""
			for chunk := range stream.Chunks {
				if chunk.Err != nil {
					t.Fatal(chunk.Err)
				}
				if format == tr.FormatClaude {
					for _, line := range strings.Split(string(chunk.Payload), "\n") {
						j := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
						if gjson.Get(j, "content_block.type").String() == "tool_use" {
							if gjson.Get(j, "content_block.name").String() != "Read" {
								t.Fatal(j)
							}
							calls++
						}
						if gjson.Get(j, "delta.stop_reason").String() == "tool_use" {
							finish = "tool_calls"
						}
					}
					continue
				}
				for _, tool := range gjson.GetBytes(chunk.Payload, "choices.0.delta.tool_calls").Array() {
					if tool.Get("index").Int() != int64(calls) || tool.Get("function.name").String() != "Read" {
						t.Fatalf("bad call: %s", tool.Raw)
					}
					calls++
				}
				if value := gjson.GetBytes(chunk.Payload, "choices.0.finish_reason").String(); value != "" {
					finish = value
				}
			}
			if calls != 2 || finish != "tool_calls" {
				t.Fatalf("calls=%d finish=%q", calls, finish)
			}
		})
	}
}

func TestCursorRequestScopedModelErrorDoesNotCoolOrRotateCredentials(t *testing.T) {
	manager := auth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(NewCursorExecutor(nil))
	reg := registry.GetGlobalRegistry()
	for _, id := range []string{"cursor-scope-first", "cursor-scope-second"} {
		reg.RegisterClient(id, "cursor", []*registry.ModelInfo{{ID: "test-model-high"}})
		t.Cleanup(func() { reg.UnregisterClient(id) })
		if _, err := manager.Register(context.Background(), cursorExecutionTestAuth(id)); err != nil {
			t.Fatal(err)
		}
	}
	attempts := 0
	ctx := cursorExecutionTestContext(t, func(_ *cp.AgentRunRequest) *http.Response {
		attempts++
		frame := cursorExecutionTestFrame(2, []byte(`{"error":{"code":"permission_denied","message":"model is not supported on your plan"}}`))
		return &http.Response{StatusCode: 200, Header: http.Header{"X-Request-Id": {"upstream-scope-test"}}, Body: io.NopCloser(bytes.NewReader(frame))}
	})
	payload := []byte(`{"messages":[{"role":"user","content":"hello"}]}`)
	_, err := manager.Execute(ctx, []string{"cursor"}, x.Request{Model: "test-model-high", Payload: payload}, x.Options{SourceFormat: tr.FormatOpenAI})
	var status *helps.CursorStatusError
	if !errors.As(err, &status) || status.StatusCode() != 400 || !status.IsRequestScoped() || attempts != 1 {
		t.Fatalf("attempts=%d error=%v", attempts, err)
	}
	for _, id := range []string{"cursor-scope-first", "cursor-scope-second"} {
		got, _ := manager.GetByID(id)
		if got.Unavailable || !got.NextRetryAfter.IsZero() {
			t.Fatalf("auth cooled: %+v", got)
		}
		for _, state := range got.ModelStates {
			if state.Unavailable || !state.NextRetryAfter.IsZero() {
				t.Fatalf("model cooled: %+v", state)
			}
		}
	}
}
