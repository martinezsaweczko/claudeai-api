package services

import (
	"crypto/rand"
	"crypto/rsa"
	"testing"
	"time"
)

func testKeys(t *testing.T) (*rsa.PrivateKey, *rsa.PublicKey) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("failed to generate test key: %v", err)
	}
	return key, &key.PublicKey
}

func TestGenerateAndVerifyToken(t *testing.T) {
	priv, pub := testKeys(t)
	svc := NewJWTService(priv, pub, "test-issuer", time.Hour)

	token, err := svc.GenerateToken("periodico")
	if err != nil {
		t.Fatalf("GenerateToken failed: %v", err)
	}

	claims, err := svc.VerifyToken(token)
	if err != nil {
		t.Fatalf("VerifyToken failed: %v", err)
	}
	if claims.Subject != "periodico" {
		t.Errorf("unexpected subject: %s", claims.Subject)
	}
	if claims.Issuer != "test-issuer" {
		t.Errorf("unexpected issuer: %s", claims.Issuer)
	}
}

func TestGenerateTokenWithExpiry(t *testing.T) {
	priv, pub := testKeys(t)
	svc := NewJWTService(priv, pub, "test-issuer", time.Hour)

	token, err := svc.GenerateTokenWithExpiry("periodico", 5*time.Minute)
	if err != nil {
		t.Fatalf("GenerateTokenWithExpiry failed: %v", err)
	}

	claims, err := svc.VerifyToken(token)
	if err != nil {
		t.Fatalf("VerifyToken failed: %v", err)
	}

	// Expiry should be ~5 minutes from now
	expected := time.Now().Add(5 * time.Minute)
	diff := claims.ExpiresAt.Time.Sub(expected)
	if diff < -time.Minute || diff > time.Minute {
		t.Errorf("expiry not ~5min: %v", claims.ExpiresAt.Time)
	}
}

func TestGenerateSubscriptionTokenHasJTI(t *testing.T) {
	priv, pub := testKeys(t)
	svc := NewJWTService(priv, pub, "test-issuer", time.Hour)

	token1, err := svc.GenerateSubscriptionToken("periodico", 48*time.Hour)
	if err != nil {
		t.Fatalf("GenerateSubscriptionToken failed: %v", err)
	}
	token2, err := svc.GenerateSubscriptionToken("periodico", 48*time.Hour)
	if err != nil {
		t.Fatalf("GenerateSubscriptionToken failed: %v", err)
	}

	if token1 == token2 {
		t.Error("subscription tokens must be unique (jti)")
	}

	claims, err := svc.VerifyToken(token1)
	if err != nil {
		t.Fatalf("VerifyToken failed: %v", err)
	}
	if claims.ID == "" {
		t.Error("missing jti claim")
	}
}

func TestVerifyTokenRejects(t *testing.T) {
	priv, pub := testKeys(t)
	svc := NewJWTService(priv, pub, "test-issuer", time.Hour)

	// Garbage
	if _, err := svc.VerifyToken("not-a-token"); err == nil {
		t.Error("expected error for garbage token")
	}

	// Token signed with a different key
	otherPriv, _ := testKeys(t)
	otherSvc := NewJWTService(otherPriv, pub, "test-issuer", time.Hour)
	token, _ := otherSvc.GenerateToken("periodico")
	if _, err := svc.VerifyToken(token); err == nil {
		t.Error("expected error for wrong-key token")
	}

	// Expired token
	expiredSvc := NewJWTService(priv, pub, "test-issuer", -time.Hour)
	token, _ = expiredSvc.GenerateToken("periodico")
	if _, err := svc.VerifyToken(token); err == nil {
		t.Error("expected error for expired token")
	}

	// Empty subject rejected at generation
	if _, err := svc.GenerateToken(""); err == nil {
		t.Error("expected error for empty subject")
	}
}
