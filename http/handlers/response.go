package handlers

import (
	"encoding/json"
	"net/http"
)

// errorResponse is a generic JSON error body.
type errorResponse struct {
	Error string `json:"error"`
}

// writeJSON writes body as JSON with the given status code.
func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
