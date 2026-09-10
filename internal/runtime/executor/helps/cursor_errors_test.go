package helps

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestCursorRequestScopedErrors(t *testing.T) {
	for _, message := range []string{"not supported in your region", "not available on your plan", "not available for your account", "model not available", "model is not supported"} {
		for _, nested := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/nested=%v", message, nested), func(t *testing.T) {
				errorBody := map[string]any{"code": "permission_denied", "message": message}
				if nested {
					errorBody["message"] = "Error"
					errorBody["details"] = []any{map[string]any{"type": "aiserver.v1.ErrorDetails", "debug": map[string]any{"error": "ERROR_MODEL_NOT_FOUND", "details": map[string]any{"detail": message}}}}
				}
				payload, _ := json.Marshal(map[string]any{"error": errorBody})
				err, ok := parseCursorConnectEnd(payload).(*CursorStatusError)
				if !ok || err.StatusCode() != 400 || !err.IsRequestScoped() || !strings.Contains(err.Error(), message) {
					t.Fatalf("error=%#v", err)
				}
			})
		}
	}
	for _, tc := range []struct {
		code   string
		status int
	}{{"permission_denied", 403}, {"unauthenticated", 401}, {"resource_exhausted", 429}, {"internal", 502}} {
		payload := fmt.Sprintf(`{"error":{"code":%q,"message":"access denied"}}`, tc.code)
		err := parseCursorConnectEnd([]byte(payload)).(*CursorStatusError)
		if err.StatusCode() != tc.status || err.IsRequestScoped() {
			t.Fatalf("credential/server error incorrectly scoped: %#v", err)
		}
	}
}

func TestCursorErrorMetadataPreservesRetryAndRequestID(t *testing.T) {
	err := parseCursorConnectEnd([]byte(`{"error":{"code":"resource_exhausted","message":"quota exhausted"},"metadata":{"retry-after":["12"],"x-request-id":["end-stream-id"]}}`)).(*CursorStatusError)
	attachCursorErrorHeaders(err, http.Header{"Retry-After": {"60"}, "X-Request-Id": {"http-id"}, "Set-Cookie": {"private"}})
	if got := err.RetryAfter(); got == nil || *got != 12*time.Second {
		t.Fatalf("retry after=%v", got)
	}
	if err.RequestID != "end-stream-id" || !strings.Contains(err.Error(), "request_id=end-stream-id") || !strings.Contains(err.Error(), "retry_after=12s") {
		t.Fatalf("diagnostics=%v", err)
	}
	headers := err.ResponseHeaders()
	if headers.Get("Retry-After") != "12" || headers.Get("Set-Cookie") != "" {
		t.Fatalf("headers=%v", headers)
	}
	headers.Set("Retry-After", "99")
	delay := err.RetryAfter()
	*delay = 0
	if err.ResponseHeaders().Get("Retry-After") != "12" || *err.RetryAfter() != 12*time.Second {
		t.Fatal("caller mutated error metadata")
	}
}

func TestCursorErrorMetadataFromDetailsAndDate(t *testing.T) {
	err := parseCursorConnectEnd([]byte(`{"error":{"code":"resource_exhausted","message":"Error","details":[{"type":"aiserver.v1.ErrorDetails","debug":{"error":"ERROR_PROVIDER_ERROR","details":{"detail":"temporarily unavailable","additionalInfo":{"providerStatusCode":429,"retryAfter":3,"request_id":"detail-id"}}}}]}}`)).(*CursorStatusError)
	if err.RequestID != "detail-id" || err.RetryAfter() == nil || *err.RetryAfter() != 3*time.Second {
		t.Fatalf("error=%v", err)
	}
	future := time.Now().Add(time.Minute).UTC().Truncate(time.Second)
	dated := attachCursorErrorHeaders(&CursorStatusError{Status: 429, Message: "limited"}, http.Header{"Retry-After": {future.Format(http.TimeFormat)}}).(*CursorStatusError)
	if delay := dated.RetryAfter(); delay == nil || *delay <= 58*time.Second || *delay > time.Minute {
		t.Fatalf("date retry=%v", delay)
	}
}

func TestCursorMalformedEndIsNotSuccessful(t *testing.T) {
	err := parseCursorConnectEnd([]byte(`{"error":`))
	if err == nil || err.(*CursorStatusError).StatusCode() != 502 {
		t.Fatalf("error=%v", err)
	}
}
