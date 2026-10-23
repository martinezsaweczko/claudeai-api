package middleware

import (
	"log/slog"
	"net/http"
	"strings"

	"github.com/martinezsaweczko/whatsappBot-golang/services"
)

// AuthorizationMiddleware creates a middleware that verifies JWT tokens from the Authorization header
// The token should be in the format: Authorization: Bearer <token>
func AuthorizationMiddleware(log *slog.Logger, jwtService *services.JWTService) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Get the Authorization header
			authHeader := r.Header.Get("Authorization")
			if authHeader == "" {
				log.Warn("Missing authorization header", "path", r.RequestURI, "method", r.Method)
				w.WriteHeader(http.StatusUnauthorized)
				w.Write([]byte(`{"error": "missing authorization header"}`))
				return
			}

			// Extract the token from the Authorization header
			parts := strings.SplitN(authHeader, " ", 2)
			if len(parts) != 2 || parts[0] != "Bearer" {
				log.Warn("Invalid authorization header format", "path", r.RequestURI, "method", r.Method)
				w.WriteHeader(http.StatusUnauthorized)
				w.Write([]byte(`{"error": "invalid authorization header format"}`))
				return
			}

			tokenString := parts[1]

			// Verify the token
			claims, err := jwtService.VerifyToken(tokenString)
			if err != nil {
				log.Warn("Invalid token", "path", r.RequestURI, "method", r.Method, "error", err.Error())
				w.WriteHeader(http.StatusUnauthorized)
				w.Write([]byte(`{"error": "invalid token"}`))
				return
			}

			// Log successful authorization
			log.Debug("Token verified successfully", "path", r.RequestURI, "method", r.Method, "subject", claims.Subject)

			// Call the next handler
			next.ServeHTTP(w, r)
		})
	}
}
