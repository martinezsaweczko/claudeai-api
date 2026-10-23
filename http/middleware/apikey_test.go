package middleware

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAPIKeyAuth(t *testing.T) {
	const keyA, keyB = "sk-local-aaaa", "sk-local-bbbb"

	tests := []struct {
		name     string
		keys     []string
		header   string
		wantCode string // empty means the request must reach the handler
	}{
		{"disabled without keys lets anonymous requests through", nil, "", ""},
		{"disabled ignores any header", []string{}, "Bearer whatever", ""},
		{"missing header", []string{keyA}, "", "missing_api_key"},
		{"token without scheme", []string{keyA}, keyA, "invalid_api_key"},
		{"wrong scheme", []string{keyA}, "Basic " + keyA, "invalid_api_key"},
		{"scheme is case sensitive", []string{keyA}, "bearer " + keyA, "invalid_api_key"},
		{"bearer with empty key", []string{keyA}, "Bearer ", "invalid_api_key"},
		{"wrong key", []string{keyA}, "Bearer sk-local-zzzz", "invalid_api_key"},
		{"key prefix is not enough", []string{keyA}, "Bearer sk-local-aaa", "invalid_api_key"},
		{"right key", []string{keyA}, "Bearer " + keyA, ""},
		{"first of several keys", []string{keyA, keyB}, "Bearer " + keyA, ""},
		{"last of several keys", []string{keyA, keyB}, "Bearer " + keyB, ""},
		{"none of several keys", []string{keyA, keyB}, "Bearer nope", "invalid_api_key"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reached := false
			next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				reached = true
				_, _ = w.Write([]byte("protected"))
			})

			req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
			if tt.header != "" {
				req.Header.Set("Authorization", tt.header)
			}
			rec := httptest.NewRecorder()
			APIKeyAuth(tt.keys)(next).ServeHTTP(rec, req)

			if tt.wantCode == "" {
				if !reached || rec.Code != http.StatusOK || rec.Body.String() != "protected" {
					t.Errorf("request should pass: reached=%v status=%d body=%q", reached, rec.Code, rec.Body.String())
				}
				return
			}

			if reached {
				t.Error("next handler must not run on an auth failure")
			}
			if rec.Code != http.StatusUnauthorized {
				t.Errorf("status = %d, want 401", rec.Code)
			}
			if got := rec.Header().Get("WWW-Authenticate"); got != "Bearer" {
				t.Errorf("WWW-Authenticate = %q, want Bearer", got)
			}
			if got := rec.Header().Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
				t.Errorf("Content-Type = %q, want JSON", got)
			}

			var body struct {
				Error struct {
					Message string `json:"message"`
					Type    string `json:"type"`
					Code    string `json:"code"`
				} `json:"error"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("body is not the OpenAI error shape: %q: %v", rec.Body.String(), err)
			}
			if body.Error.Code != tt.wantCode || body.Error.Type != "invalid_request_error" || body.Error.Message == "" {
				t.Errorf("error body = %+v, want code %q and type invalid_request_error", body.Error, tt.wantCode)
			}

			// The supplied credential must never be echoed back.
			if supplied := strings.TrimPrefix(tt.header, "Bearer "); len(supplied) > 3 && strings.Contains(rec.Body.String(), supplied) {
				t.Errorf("response echoes the supplied key: %s", rec.Body.String())
			}
		})
	}
}

func TestWriteOpenAIError(t *testing.T) {
	rec := httptest.NewRecorder()
	WriteOpenAIError(rec, http.StatusBadRequest, "bad thing", "invalid_request_error", "bad_thing")

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
	want := `{"error":{"message":"bad thing","type":"invalid_request_error","param":null,"code":"bad_thing"}}`
	if got := strings.TrimSpace(rec.Body.String()); got != want {
		t.Errorf("body = %s, want %s", got, want)
	}
}
