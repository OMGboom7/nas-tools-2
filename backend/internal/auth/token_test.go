package auth

import (
	"errors"
	"testing"
	"time"
)

func TestTokenManagerIssueVerifyAndRevoke(t *testing.T) {
	manager, err := NewTokenManager("test-secret")
	if err != nil {
		t.Fatalf("NewTokenManager() error = %v", err)
	}
	now := time.Unix(1_700_000_000, 0)
	manager.now = func() time.Time { return now }
	token, err := manager.Issue("admin", 2*time.Hour)
	if err != nil {
		t.Fatalf("Issue() error = %v", err)
	}
	claims, err := manager.Verify(token)
	if err != nil || claims.Username != "admin" {
		t.Fatalf("Verify() = %#v, %v", claims, err)
	}
	manager.Revoke(token)
	if _, err := manager.Verify(token); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("Verify(revoked) error = %v", err)
	}
}

func TestTokenManagerRejectsExpiredAndTamperedTokens(t *testing.T) {
	manager, _ := NewTokenManager("test-secret")
	now := time.Unix(1_700_000_000, 0)
	manager.now = func() time.Time { return now }
	token, _ := manager.Issue("admin", time.Minute)
	manager.now = func() time.Time { return now.Add(2 * time.Minute) }
	if _, err := manager.Verify(token); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("expired Verify() error = %v", err)
	}
	if _, err := manager.Verify(token + "x"); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("tampered Verify() error = %v", err)
	}
}
