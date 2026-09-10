package helps

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps/cursorproto"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"
)

func marshalCursorTestToolFrame(t *testing.T, id string) []byte {
	t.Helper()
	message, err := proto.Marshal(&cursorproto.AgentServerMessage{
		Message: &cursorproto.AgentServerMessage_ExecServerMessage{ExecServerMessage: &cursorproto.ExecServerMessage{
			Message: &cursorproto.ExecServerMessage_McpArgs{McpArgs: &cursorproto.McpArgs{ToolCallId: id, ToolName: "Read"}},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	frame := make([]byte, 5+len(message))
	binary.BigEndian.PutUint32(frame[1:5], uint32(len(message)))
	copy(frame[5:], message)
	return frame
}

func TestCursorToolBatchCollectsDelayedCallsAndStopsWhenIdle(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		body, source := io.Pipe()
		defer source.Close()
		events := make(chan CursorStreamEvent, 16)
		go runCursorResponseLoop(context.Background(), body, &cursorRequestWriter{}, &CursorRunPayload{}, events, nil)
		write := func(id string) {
			t.Helper()
			if _, err := source.Write(marshalCursorTestToolFrame(t, id)); err != nil {
				t.Fatal(err)
			}
			synctest.Wait()
		}
		write("first")
		// Sleep advances virtual time only; the second call must extend the idle window.
		time.Sleep(cursorToolBatchIdle * 3 / 4)
		// Streaming activity between calls must keep the batch open as well.
		if _, err := source.Write(marshalCursorTestTextFrame(t, "reading both files")); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		time.Sleep(cursorToolBatchIdle * 3 / 4)
		write("second")
		write("second")
		time.Sleep(cursorToolBatchIdle * 3 / 4)
		write("third")
		time.Sleep(cursorToolBatchIdle)
		synctest.Wait()
		var ids []string
		done, usage := 0, 0
		for event := range events {
			if event.Err != nil {
				t.Fatal(event.Err)
			}
			if event.ToolCallID != "" {
				ids = append(ids, event.ToolCallID)
			}
			if event.Done {
				done++
			}
			if event.Usage {
				usage++
			}
		}
		if strings.Join(ids, ",") != "first,second,third" || done != 1 || usage != 1 {
			t.Fatalf("calls=%v done=%d usage=%d", ids, done, usage)
		}
		if _, err := source.Write([]byte{0}); err == nil {
			t.Fatal("upstream reader was not closed after batch completion")
		}
	})
}

func TestCursorToolBatchPropagatesErrorAfterFirstCall(t *testing.T) {
	payload := []byte(`{"error":{"code":"permission_denied","message":"access denied"}}`)
	frames := append(marshalCursorTestToolFrame(t, "first"), cursorConnectEndStreamFlag, 0, 0, 0, byte(len(payload)))
	frames = append(frames, payload...)
	events := make(chan CursorStreamEvent, 8)
	runCursorResponseLoop(context.Background(), io.NopCloser(bytes.NewReader(frames)), &cursorRequestWriter{}, &CursorRunPayload{}, events, http.Header{"X-Request-Id": {"upstream-test"}})
	calls, failures, done := 0, 0, 0
	for event := range events {
		if event.ToolCallID != "" {
			calls++
		}
		if event.Done {
			done++
		}
		if event.Err != nil {
			failures++
			err, ok := event.Err.(*CursorStatusError)
			if !ok || err.StatusCode() != 403 || !strings.Contains(err.Error(), "upstream-test") {
				t.Fatalf("error=%v", event.Err)
			}
		}
	}
	if calls != 1 || failures != 1 || done != 0 {
		t.Fatalf("calls=%d failures=%d done=%d", calls, failures, done)
	}
}

func TestCursorToolBatchCancellationClosesBlockedReader(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		body, source := io.Pipe()
		defer source.Close()
		events := make(chan CursorStreamEvent, 8)
		go runCursorResponseLoop(ctx, body, &cursorRequestWriter{}, &CursorRunPayload{}, events, nil)
		if _, err := source.Write(marshalCursorTestToolFrame(t, "first")); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		cancel()
		synctest.Wait()
		for event := range events {
			if event.Done || event.Err != nil {
				t.Fatalf("unexpected cancellation event: %+v", event)
			}
		}
		if _, err := source.Write([]byte{0}); err == nil {
			t.Fatal("reader remains open")
		}
	})
}

func TestRespondCursorExecRestoresClientToolName(t *testing.T) {
	longName := strings.Repeat("long_tool", 8)
	tools := []*cursorproto.McpToolDefinition{
		{Name: cursorMCPToolName("Read"), ToolName: "Read"},
		{Name: cursorMCPToolName("mcp__cliproxy__Read"), ToolName: "mcp__cliproxy__Read"},
		{Name: cursorMCPToolName(longName), ToolName: longName},
	}
	for _, tc := range []struct{ name, toolName, want string }{
		{cursorMCPToolName("Read"), "Read", "Read"},
		{cursorMCPToolName("Read"), "", "Read"},
		{cursorMCPToolName(longName), "", longName},
		{cursorMCPToolName("mcp__cliproxy__Read"), "mcp__cliproxy__Read", "mcp__cliproxy__Read"},
		{"legacy_tool", "", "legacy_tool"},
	} {
		t.Run(tc.name+"/"+tc.toolName, func(t *testing.T) {
			value, err := proto.Marshal(structpb.NewStringValue("/tmp/example"))
			if err != nil {
				t.Fatal(err)
			}
			event, err := respondCursorExec(nil, tools, "", &cursorproto.ExecServerMessage{
				Message: &cursorproto.ExecServerMessage_McpArgs{McpArgs: &cursorproto.McpArgs{
					Name: tc.name, ToolName: tc.toolName, ToolCallId: "call_1", Args: map[string][]byte{"path": value},
				}},
			})
			if err != nil {
				t.Fatal(err)
			}
			if event.ToolName != tc.want || event.ToolCallID != "call_1" || event.ToolArguments != `{"path":"/tmp/example"}` {
				t.Fatalf("tool call changed: %+v", event)
			}
		})
	}
}

func TestParseCursorConnectEndProviderError(t *testing.T) {
	for _, tc := range []struct {
		name, code string
		wantStatus int
	}{
		{"string bad request", `"400"`, 400},
		{"numeric bad request", `400`, 400},
		{"rate limit", `"429"`, 429},
		{"unavailable", `"503"`, 503},
		{"bad gateway", `"502"`, 502},
		{"missing status", `null`, 502},
		{"invalid status", `"unknown"`, 502},
		{"non-error status", `"200"`, 502},
	} {
		t.Run(tc.name, func(t *testing.T) {
			payload := fmt.Sprintf(`{"error":{"code":"resource_exhausted","message":"Error","details":[{"type":"aiserver.v1.ErrorDetails","debug":{"error":"ERROR_PROVIDER_ERROR","details":{"title":"Provider Error","detail":"The provider rejected the request.","additionalInfo":{"providerStatusCode":%s}}}}]}}`, tc.code)
			err := parseCursorConnectEnd([]byte(payload))
			statusErr, ok := err.(*CursorStatusError)
			if !ok || statusErr.StatusCode() != tc.wantStatus {
				t.Fatalf("error = %#v, want status %d", err, tc.wantStatus)
			}
			for _, want := range []string{"ERROR_PROVIDER_ERROR", "Provider Error", "The provider rejected the request."} {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("error %q does not contain %q", err, want)
				}
			}
			if tc.wantStatus == 400 && !strings.Contains(err.Error(), "provider HTTP 400") {
				t.Fatalf("provider status is missing from error: %v", err)
			}
		})
	}
}

func TestParseCursorConnectEndKeepsQuotaClassification(t *testing.T) {
	for _, errorType := range []string{"ERROR_USAGE_LIMIT", ""} {
		payload, err := json.Marshal(map[string]any{"error": map[string]any{
			"code": "resource_exhausted", "message": "quota exhausted",
			"details": []any{map[string]any{"type": "aiserver.v1.ErrorDetails", "debug": map[string]any{
				"error": errorType, "details": map[string]any{"additionalInfo": map[string]any{"providerStatusCode": "400"}},
			}}},
		}})
		if err != nil {
			t.Fatal(err)
		}
		statusErr, ok := parseCursorConnectEnd(payload).(*CursorStatusError)
		if !ok || statusErr.StatusCode() != 429 || statusErr.IsRequestScoped() || !strings.Contains(statusErr.Error(), "quota exhausted") {
			t.Fatalf("quota error changed: %#v", statusErr)
		}
	}
}

func TestApplyCursorRunHeadersRequestsStreaming(t *testing.T) {
	request, errRequest := http.NewRequest(http.MethodPost, "https://example.com/run", nil)
	if errRequest != nil {
		t.Fatalf("create request: %v", errRequest)
	}

	applyCursorRunHeaders(request, "token")

	if got := request.Header.Get("X-Cursor-Streaming"); got != "true" {
		t.Fatalf("X-Cursor-Streaming = %q, want %q", got, "true")
	}
}

func TestRunCursorResponseLoopEmitsTextFramesIncrementally(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	responseReader, responseWriter := io.Pipe()
	requestReader, requestWriter := io.Pipe()
	defer requestReader.Close()
	writer := &cursorRequestWriter{pipe: requestWriter}
	events := make(chan CursorStreamEvent)
	run := &CursorRunPayload{Blobs: make(map[string][]byte)}
	go runCursorResponseLoop(ctx, responseReader, writer, run, events, nil)

	firstFrame := marshalCursorTestTextFrame(t, "first")
	secondFrame := marshalCursorTestTextFrame(t, "second")
	firstWritten := make(chan struct{})
	releaseSecond := make(chan struct{})
	writeDone := make(chan error, 1)
	go func() {
		if _, errWrite := responseWriter.Write(firstFrame); errWrite != nil {
			writeDone <- errWrite
			return
		}
		close(firstWritten)
		select {
		case <-releaseSecond:
		case <-ctx.Done():
			writeDone <- ctx.Err()
			return
		}
		if _, errWrite := responseWriter.Write(secondFrame); errWrite != nil {
			writeDone <- errWrite
			return
		}
		writeDone <- responseWriter.Close()
	}()

	select {
	case <-firstWritten:
	case <-time.After(time.Second):
		t.Fatal("response loop did not consume the first frame")
	}
	select {
	case event := <-events:
		if event.Text != "first" {
			t.Fatalf("first event text = %q, want %q", event.Text, "first")
		}
	case <-time.After(time.Second):
		t.Fatal("first text frame was buffered instead of emitted immediately")
	}

	close(releaseSecond)
	select {
	case event := <-events:
		if event.Text != "second" {
			t.Fatalf("second event text = %q, want %q", event.Text, "second")
		}
	case <-time.After(time.Second):
		t.Fatal("second text frame was not emitted")
	}

	if errWrite := <-writeDone; errWrite != nil {
		t.Fatalf("write response frames: %v", errWrite)
	}
}

func marshalCursorTestTextFrame(t *testing.T, text string) []byte {
	t.Helper()
	message, errMarshal := proto.Marshal(&cursorproto.AgentServerMessage{
		Message: &cursorproto.AgentServerMessage_InteractionUpdate{
			InteractionUpdate: &cursorproto.InteractionUpdate{
				Message: &cursorproto.InteractionUpdate_TextDelta{
					TextDelta: &cursorproto.TextDeltaUpdate{Text: text},
				},
			},
		},
	})
	if errMarshal != nil {
		t.Fatalf("marshal text frame: %v", errMarshal)
	}
	frame := bytes.NewBuffer(make([]byte, 0, len(message)+5))
	frame.WriteByte(0)
	if errLength := binary.Write(frame, binary.BigEndian, uint32(len(message))); errLength != nil {
		t.Fatalf("write frame length: %v", errLength)
	}
	frame.Write(message)
	return frame.Bytes()
}
