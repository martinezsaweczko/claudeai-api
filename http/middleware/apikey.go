package middleware

import (
	"crypto/sha256"
	"crypto/subtle"
	"net/http"
	"strings"
)

// APIKeyAuth returns a middleware that requires "Authorization: Bearer <key>"
// with one of the given keys. With no keys configured authentication is
// disabled and the middleware is a pass-through. Keys are compared as SHA-256
// digests with a constant-time comparison, and every key is checked so the
// response time does not reveal which key matched. The supplied credential is
// never logged or echoed.
func APIKeyAuth(keys []string) Middleware {
	if len(keys) == 0 {
		return func(next http.Handler) http.Handler { return next }
	}

	digests := make([][sha256.Size]byte, len(keys))
	for i, k := range keys {
		digests[i] = sha256.Sum256([]byte(k))
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			header := r.Header.Get("Authorization")
			if header == "" {
				unauthorized(w, "missing_api_key", "Missing API key: send an 'Authorization: Bearer <key>' header.")
				return
			}

			scheme, key, found := strings.Cut(header, " ")
			if !found || scheme != "Bearer" || key == "" {
				unauthorized(w, "invalid_api_key", "Invalid API key: expected 'Authorization: Bearer <key>'.")
				return
			}

			got := sha256.Sum256([]byte(key))
			match := 0
			for i := range digests {
				match |= subtle.ConstantTimeCompare(got[:], digests[i][:])
			}
			if match != 1 {
				unauthorized(w, "invalid_api_key", "Invalid API key.")
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}

func unauthorized(w http.ResponseWriter, code, message string) {
	w.Header().Set("WWW-Authenticate", "Bearer")
	WriteOpenAIError(w, http.StatusUnauthorized, message, ErrorTypeInvalidRequest, code)
}
