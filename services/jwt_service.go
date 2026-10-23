package services

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/hex"
	"fmt"
	"log/slog"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// randomHex returns n random bytes hex-encoded
func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// Claims represents the JWT claims
type Claims struct {
	jwt.RegisteredClaims
	Subject string `json:"sub"`
}

// JWTService provides JWT token creation and verification
type JWTService struct {
	privateKey *rsa.PrivateKey
	publicKey  *rsa.PublicKey
	issuer     string
	expiryTime time.Duration
	log        *slog.Logger
}

// NewJWTService creates a new JWT service with the given keys
func NewJWTService(privateKey *rsa.PrivateKey, publicKey *rsa.PublicKey, issuer string, expiryTime time.Duration) *JWTService {
	return &JWTService{
		privateKey: privateKey,
		publicKey:  publicKey,
		issuer:     issuer,
		log:        slog.Default(),
		expiryTime: expiryTime,
	}
}

// GenerateToken generates a new JWT token with the given subject
func (s *JWTService) GenerateToken(subject string) (string, error) {
	return s.GenerateTokenWithExpiry(subject, s.expiryTime)
}

// GenerateTokenWithExpiry generates a new JWT token with a custom expiry time
func (s *JWTService) GenerateTokenWithExpiry(subject string, expiry time.Duration) (string, error) {
	if subject == "" {
		return "", fmt.Errorf("subject cannot be empty")
	}

	now := time.Now()

	claims := &Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(now.Add(expiry)),
			IssuedAt:  jwt.NewNumericDate(now),
			Issuer:    s.issuer,
		},
		Subject: subject,
	}

	token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	tokenString, err := token.SignedString(s.privateKey)
	if err != nil {
		return "", fmt.Errorf("failed to sign token: %w", err)
	}

	return tokenString, nil
}

// GenerateSubscriptionToken generates a JWT token with a unique ID (jti claim),
// used for subscription download links (48h validity)
func (s *JWTService) GenerateSubscriptionToken(subject string, expiry time.Duration) (string, error) {
	if subject == "" {
		return "", fmt.Errorf("subject cannot be empty")
	}

	now := time.Now()
	jti, err := randomHex(6)
	if err != nil {
		return "", fmt.Errorf("failed to generate token id: %w", err)
	}

	claims := &Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(now.Add(expiry)),
			IssuedAt:  jwt.NewNumericDate(now),
			Issuer:    s.issuer,
			ID:        jti,
		},
		Subject: subject,
	}

	token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	tokenString, err := token.SignedString(s.privateKey)
	if err != nil {
		return "", fmt.Errorf("failed to sign token: %w", err)
	}

	return tokenString, nil
}

// VerifyToken verifies a JWT token and returns the claims if valid
func (s *JWTService) VerifyToken(tokenString string) (*Claims, error) {
	claims := &Claims{}
	s.log.Debug("Starting token verification")

	token, err := jwt.ParseWithClaims(tokenString, claims, func(token *jwt.Token) (interface{}, error) {
		// Verify the signing method
		signingMethod := fmt.Sprintf("%v", token.Header["alg"])
		s.log.Debug("Verifying signing method", "method", signingMethod)

		if _, ok := token.Method.(*jwt.SigningMethodRSA); !ok {
			s.log.Error("Invalid signing method", "method", signingMethod, "expected", "RS256")
			return nil, fmt.Errorf("unexpected signing method: %v", token.Header["alg"])
		}

		s.log.Debug("Signing method verified", "method", signingMethod)
		return s.publicKey, nil
	})

	if err != nil {
		s.log.Error("Failed to parse token", "error", err.Error())
		return nil, fmt.Errorf("failed to parse token: %w", err)
	}

	s.log.Debug("Token parsed successfully", "valid", token.Valid)

	if !token.Valid {
		s.log.Error("Token is invalid", "registered_claims", fmt.Sprintf("%+v", claims.RegisteredClaims))
		return nil, fmt.Errorf("invalid token")
	}

	// Additional validation
	now := time.Now()
	if claims.ExpiresAt != nil {
		expiryTime := claims.ExpiresAt.Time
		s.log.Debug("Token expiration check", "expires_at", expiryTime, "now", now, "expired", expiryTime.Before(now))
	}

	if claims.IssuedAt != nil {
		issuedTime := claims.IssuedAt.Time
		s.log.Debug("Token issued at", "issued_at", issuedTime)
	}

	s.log.Debug("Token verified successfully", "subject", claims.Subject, "issuer", claims.Issuer)

	return claims, nil
}
