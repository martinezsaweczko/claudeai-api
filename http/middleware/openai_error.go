package middleware

import (
	"encoding/json"
	"net/http"
)

// OpenAI error types used by the API.
const (
	ErrorTypeInvalidRequest = "invalid_request_error"
	ErrorTypeAPI            = "api_error"
	ErrorTypeRateLimit      = "rate_limit_error"
)

type openAIErrorBody struct {
	Error openAIError `json:"error"`
}

type openAIError struct {
	Message string  `json:"message"`
	Type    string  `json:"type"`
	Param   *string `json:"param"`
	Code    string  `json:"code"`
}

// WriteOpenAIError writes an OpenAI-style JSON error body with the given status.
// The "param" field is always null. It lives in the middleware package so
// middlewares can reuse it without importing the handlers package.
func WriteOpenAIError(w http.ResponseWriter, status int, message, errType, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(openAIErrorBody{Error: openAIError{Message: message, Type: errType, Code: code}})
}
