package auth

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"golang.org/x/crypto/scrypt"
)

func TestVerifyPasswordPBKDF2(t *testing.T) {
	encoded := "pbkdf2:sha256:260000$nastool1$cff28e0a04428d3e410b457950db9fc19bdb08a263d337c782e955bd48afd824"
	if !VerifyPassword(encoded, "correct horse battery staple") {
		t.Fatal("VerifyPassword() rejected valid PBKDF2 password")
	}
	if VerifyPassword(encoded, "wrong") {
		t.Fatal("VerifyPassword() accepted invalid PBKDF2 password")
	}
}

func TestVerifyPasswordScryptAndConfigPrefix(t *testing.T) {
	digest, err := scrypt.Key([]byte("password"), []byte("fixedsalt"), 1<<14, 8, 1, sha256.Size)
	if err != nil {
		t.Fatalf("scrypt fixture: %v", err)
	}
	encoded := "[hash]scrypt:16384:8:1$fixedsalt$" + hex.EncodeToString(digest)
	if !VerifyPassword(encoded, "password") {
		t.Fatal("VerifyPassword() rejected valid scrypt password")
	}
	if VerifyPassword(encoded, "wrong") {
		t.Fatal("VerifyPassword() accepted invalid scrypt password")
	}
}

func TestVerifyPasswordPlainBootstrapValue(t *testing.T) {
	if !VerifyPassword("password", "password") || VerifyPassword("password", "other") {
		t.Fatal("plain bootstrap password compatibility failed")
	}
}

func TestHashPasswordUsesWerkzeugCompatibleScrypt(t *testing.T) {
	encoded, err := HashPassword("new-password")
	if err != nil {
		t.Fatalf("HashPassword() error = %v", err)
	}
	if !VerifyPassword(encoded, "new-password") {
		t.Fatal("generated hash could not be verified")
	}
	if NeedsPasswordRehash(encoded) {
		t.Fatalf("NeedsPasswordRehash(%q) = true", encoded)
	}
	if !NeedsPasswordRehash("pbkdf2:sha256:260000$salt$digest") {
		t.Fatal("legacy PBKDF2 hash was not marked for upgrade")
	}
}
