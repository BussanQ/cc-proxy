package helps

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// CursorStatusError is an HTTP-like error returned by Cursor's Connect stream.
type CursorStatusError struct {
	Status        int
	Message       string
	RequestScoped bool
	RequestID     string
	Headers       http.Header
	retryAfter    *time.Duration
}

func (e *CursorStatusError) Error() string {
	if e == nil {
		return "cursor upstream error"
	}
	message := e.Message
	if e.RequestID != "" {
		message += " (request_id=" + e.RequestID + ")"
	}
	if e.retryAfter != nil {
		message += " (retry_after=" + e.retryAfter.String() + ")"
	}
	return message
}

func (e *CursorStatusError) IsRequestScoped() bool { return e != nil && e.RequestScoped }

func (e *CursorStatusError) ResponseHeaders() http.Header {
	if e == nil {
		return nil
	}
	return e.Headers.Clone()
}

func (e *CursorStatusError) RetryAfter() *time.Duration {
	if e == nil || e.retryAfter == nil {
		return nil
	}
	value := *e.retryAfter
	return &value
}

func (e *CursorStatusError) StatusCode() int {
	if e == nil {
		return 0
	}
	return e.Status
}

type cursorConnectErrorDetail struct {
	Type  string `json:"type"`
	Debug struct {
		Error   string `json:"error"`
		Details struct {
			Title          string                     `json:"title"`
			Detail         string                     `json:"detail"`
			RequestID      string                     `json:"requestId"`
			AdditionalInfo map[string]json.RawMessage `json:"additionalInfo"`
		} `json:"details"`
	} `json:"debug"`
}

func parseCursorConnectEnd(payload []byte) error {
	if len(bytes.TrimSpace(payload)) == 0 {
		return nil
	}
	var envelope struct {
		Error *struct {
			Code    string            `json:"code"`
			Message string            `json:"message"`
			Details []json.RawMessage `json:"details"`
		} `json:"error"`
		Metadata map[string][]string `json:"metadata"`
	}
	if err := json.Unmarshal(payload, &envelope); err != nil {
		return &CursorStatusError{Status: http.StatusBadGateway, Message: "cursor stream: invalid end-stream payload: " + err.Error()}
	}
	if envelope.Error == nil {
		return nil
	}
	message := strings.TrimSpace(envelope.Error.Message)
	if message == "" {
		message = "Cursor upstream error"
	}
	status := http.StatusBadGateway
	switch strings.ToLower(strings.TrimSpace(envelope.Error.Code)) {
	case "unauthenticated":
		status = http.StatusUnauthorized
	case "permission_denied":
		status = http.StatusForbidden
	case "resource_exhausted":
		if cursorContextError(message) {
			status = http.StatusBadRequest
		} else {
			status = http.StatusTooManyRequests
		}
	case "invalid_argument":
		status = http.StatusBadRequest
	case "unavailable":
		status = http.StatusServiceUnavailable
	case "deadline_exceeded":
		status = http.StatusGatewayTimeout
	}
	headers := make(http.Header)
	for key, values := range envelope.Metadata {
		for _, value := range values {
			headers.Add(key, value)
		}
	}
	for _, raw := range envelope.Error.Details {
		var detail cursorConnectErrorDetail
		if json.Unmarshal(raw, &detail) != nil || detail.Type != "aiserver.v1.ErrorDetails" {
			continue
		}
		parts := []string{}
		if detail.Debug.Error != "" {
			parts = append(parts, detail.Debug.Error)
		}
		if message != "Error" && message != "Cursor upstream error" {
			parts = append(parts, message)
		}
		for _, text := range []string{detail.Debug.Details.Title, detail.Debug.Details.Detail} {
			if text = strings.TrimSpace(text); text != "" {
				parts = append(parts, text)
			}
		}
		if len(parts) > 0 {
			message = strings.Join(parts, ": ")
		}
		additional := detail.Debug.Details.AdditionalInfo
		if detail.Debug.Error == "ERROR_PROVIDER_ERROR" {
			// resource_exhausted can wrap a provider's HTTP 400; this is not an account quota error.
			status = http.StatusBadGateway
			if code, err := strconv.Atoi(cursorErrorDetailString(additional["providerStatusCode"])); err == nil && code >= 400 && code <= 599 {
				status = code
				message += fmt.Sprintf(" (provider HTTP %d)", code)
			}
		}
		if cursorRequestID(headers) == "" {
			id := detail.Debug.Details.RequestID
			for _, key := range []string{"requestId", "request_id", "requestID"} {
				if id == "" {
					id = cursorErrorDetailString(additional[key])
				}
			}
			if id != "" {
				headers.Set("X-Request-ID", id)
			}
		}
		if headers.Get("Retry-After") == "" {
			for _, key := range []string{"retryAfter", "retry_after", "retry-after"} {
				if value := cursorErrorDetailString(additional[key]); value != "" {
					headers.Set("Retry-After", value)
					break
				}
			}
		}
	}
	requestScoped := cursorRequestErrorMessage(message)
	if requestScoped {
		status = http.StatusBadRequest
	}
	return attachCursorErrorHeaders(&CursorStatusError{Status: status, Message: "cursor stream: " + message, RequestScoped: requestScoped}, headers)
}

func cursorErrorDetailString(raw json.RawMessage) string {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return strings.TrimSpace(text)
	}
	var number json.Number
	if json.Unmarshal(raw, &number) == nil {
		return number.String()
	}
	return ""
}

func cursorRequestErrorMessage(message string) bool {
	lower := strings.ToLower(message)
	for _, marker := range []string{
		"not supported in your region", "not available in your region", "unsupported country",
		"not available on your plan", "not available for your plan", "not available for your account",
		"model not available", "model is not available", "model unavailable", "requested model is unavailable",
		"model not supported", "model is not supported", "model_not_supported", "unsupported model",
	} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

func cursorContextError(message string) bool {
	lower := strings.ToLower(message)
	for _, marker := range []string{"context", "token", "length", "overflow", "too long", "too large"} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

func cursorRequestID(headers http.Header) string {
	for _, name := range []string{"X-Request-ID", "Request-ID", "X-Cursor-Request-ID"} {
		if value := strings.TrimSpace(headers.Get(name)); value != "" {
			return value
		}
	}
	return ""
}

func attachCursorErrorHeaders(err error, headers http.Header) error {
	var statusErr *CursorStatusError
	if !errors.As(err, &statusErr) {
		return err
	}
	combined := make(http.Header)
	// Retain only diagnostic headers; do not forward arbitrary upstream headers on errors.
	for _, source := range []http.Header{headers, statusErr.Headers} {
		for _, name := range []string{"Retry-After", "X-Request-ID", "Request-ID", "X-Cursor-Request-ID"} {
			if value := source.Get(name); value != "" {
				combined.Set(name, value)
			}
		}
	}
	statusErr.Headers = combined
	if statusErr.RequestID == "" {
		statusErr.RequestID = cursorRequestID(combined)
	}
	if statusErr.RequestID != "" {
		combined.Set("X-Request-ID", statusErr.RequestID)
	}
	if raw := combined.Get("Retry-After"); raw != "" {
		now := time.Now()
		if next, ok := parseRetryAfterHeader(raw, now); ok {
			delay := next.Sub(now)
			if delay < 0 {
				delay = 0
			}
			statusErr.retryAfter = &delay
		}
	}
	return statusErr
}
