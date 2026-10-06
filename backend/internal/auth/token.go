package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

var ErrInvalidToken = errors.New("invalid or expired token")

type Claims struct {
	Username string `json:"username"`
	IssuedAt int64  `json:"iat"`
	Expires  int64  `json:"exp"`
}

type TokenManager struct {
	secret  []byte
	now     func() time.Time
	mu      sync.RWMutex
	revoked map[string]int64
}

func NewTokenManager(secret string) (*TokenManager, error) {
	if secret == "" {
		return nil, errors.New("token secret is required")
	}
	return &TokenManager{secret: []byte(secret), now: time.Now, revoked: make(map[string]int64)}, nil
}

func (manager *TokenManager) Issue(username string, lifetime time.Duration) (string, error) {
	if username == "" || lifetime <= 0 {
		return "", errors.New("username and positive token lifetime are required")
	}
	now := manager.now().UTC()
	header, _ := json.Marshal(map[string]string{"alg": "HS256", "typ": "JWT"})
	payload, err := json.Marshal(Claims{Username: username, IssuedAt: now.Unix(), Expires: now.Add(lifetime).Unix()})
	if err != nil {
		return "", fmt.Errorf("encode token claims: %w", err)
	}
	unsigned := rawURL(header) + "." + rawURL(payload)
	return unsigned + "." + rawURL(manager.sign(unsigned)), nil
}

func (manager *TokenManager) Verify(token string) (Claims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return Claims{}, ErrInvalidToken
	}
	unsigned := parts[0] + "." + parts[1]
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || subtle.ConstantTimeCompare(signature, manager.sign(unsigned)) != 1 {
		return Claims{}, ErrInvalidToken
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return Claims{}, ErrInvalidToken
	}
	var claims Claims
	if err := json.Unmarshal(payload, &claims); err != nil || claims.Username == "" {
		return Claims{}, ErrInvalidToken
	}
	now := manager.now().Unix()
	if claims.Expires <= now || claims.IssuedAt > now+60 {
		return Claims{}, ErrInvalidToken
	}
	manager.mu.RLock()
	revokedUntil, revoked := manager.revoked[token]
	manager.mu.RUnlock()
	if revoked && revokedUntil > now {
		return Claims{}, ErrInvalidToken
	}
	return claims, nil
}

func (manager *TokenManager) Revoke(token string) {
	claims, err := manager.Verify(token)
	if err != nil {
		return
	}
	manager.mu.Lock()
	defer manager.mu.Unlock()
	now := manager.now().Unix()
	for existing, expires := range manager.revoked {
		if expires <= now {
			delete(manager.revoked, existing)
		}
	}
	manager.revoked[token] = claims.Expires
}

func (manager *TokenManager) sign(unsigned string) []byte {
	mac := hmac.New(sha256.New, manager.secret)
	_, _ = mac.Write([]byte(unsigned))
	return mac.Sum(nil)
}

func rawURL(value []byte) string {
	return base64.RawURLEncoding.EncodeToString(value)
}
