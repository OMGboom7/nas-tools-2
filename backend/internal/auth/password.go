package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"golang.org/x/crypto/pbkdf2"
	"golang.org/x/crypto/scrypt"
)

const (
	scryptN = 32768
	scryptR = 8
	scryptP = 1
)

func HashPassword(password string) (string, error) {
	if password == "" {
		return "", errors.New("password is required")
	}
	saltBytes := make([]byte, 16)
	if _, err := rand.Read(saltBytes); err != nil {
		return "", fmt.Errorf("generate password salt: %w", err)
	}
	salt := base64.RawURLEncoding.EncodeToString(saltBytes)
	digest, err := scrypt.Key([]byte(password), []byte(salt), scryptN, scryptR, scryptP, sha256.Size)
	if err != nil {
		return "", fmt.Errorf("hash password: %w", err)
	}
	return fmt.Sprintf("scrypt:%d:%d:%d$%s$%s", scryptN, scryptR, scryptP, salt, hex.EncodeToString(digest)), nil
}

func NeedsPasswordRehash(encoded string) bool {
	encoded = strings.TrimPrefix(encoded, "[hash]")
	return !strings.HasPrefix(encoded, fmt.Sprintf("scrypt:%d:%d:%d$", scryptN, scryptR, scryptP))
}

// VerifyPassword supports the password formats emitted by Werkzeug 2.x. The
// [hash] prefix is NAS Tools' config.yaml marker and is not part of the hash.
func VerifyPassword(encoded, password string) bool {
	encoded = strings.TrimPrefix(encoded, "[hash]")
	parts := strings.Split(encoded, "$")
	if len(parts) != 3 {
		return subtle.ConstantTimeCompare([]byte(encoded), []byte(password)) == 1
	}

	want, err := hex.DecodeString(parts[2])
	if err != nil || len(want) == 0 {
		return false
	}

	var got []byte
	switch method := strings.Split(parts[0], ":"); method[0] {
	case "pbkdf2":
		if len(method) != 3 || method[1] != "sha256" {
			return false
		}
		iterations, err := positiveInt(method[2])
		if err != nil {
			return false
		}
		got = pbkdf2.Key([]byte(password), []byte(parts[1]), iterations, len(want), sha256.New)
	case "scrypt":
		if len(method) != 4 {
			return false
		}
		n, errN := positiveInt(method[1])
		r, errR := positiveInt(method[2])
		p, errP := positiveInt(method[3])
		if errN != nil || errR != nil || errP != nil {
			return false
		}
		got, err = scrypt.Key([]byte(password), []byte(parts[1]), n, r, p, len(want))
		if err != nil {
			return false
		}
	default:
		return false
	}

	return subtle.ConstantTimeCompare(got, want) == 1
}

func positiveInt(value string) (int, error) {
	number, err := strconv.Atoi(value)
	if err != nil || number <= 0 {
		return 0, fmt.Errorf("invalid positive integer %q", value)
	}
	return number, nil
}
