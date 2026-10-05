package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

type Role string

const (
	RoleUser  Role = "user"
	RoleAdmin Role = "admin"
)

type Principal struct {
	UserID string
	Role   Role
}

type TokenManager struct {
	secret []byte
	issuer string
	ttl    time.Duration
}

type tokenHeader struct {
	Algorithm string `json:"alg"`
	Type      string `json:"typ"`
}

type tokenClaims struct {
	Subject   string `json:"sub"`
	Role      Role   `json:"role"`
	Issuer    string `json:"iss"`
	IssuedAt  int64  `json:"iat"`
	ExpiresAt int64  `json:"exp"`
}

func NewTokenManager(secret string, issuer string, ttl time.Duration) (*TokenManager, error) {
	if len(secret) < 32 {
		return nil, errors.New("JWT_SECRET must contain at least 32 characters")
	}
	if issuer == "" {
		return nil, errors.New("token issuer is required")
	}
	if ttl <= 0 {
		return nil, errors.New("token lifetime must be positive")
	}

	return &TokenManager{
		secret: []byte(secret),
		issuer: issuer,
		ttl:    ttl,
	}, nil
}

func (manager *TokenManager) Issue(userID string, role Role) (string, error) {
	now := time.Now().UTC()
	header := tokenHeader{Algorithm: "HS256", Type: "JWT"}
	claims := tokenClaims{
		Subject:   userID,
		Role:      role,
		Issuer:    manager.issuer,
		IssuedAt:  now.Unix(),
		ExpiresAt: now.Add(manager.ttl).Unix(),
	}

	encodedHeader, err := encodeSegment(header)
	if err != nil {
		return "", fmt.Errorf("encode token header: %w", err)
	}
	encodedClaims, err := encodeSegment(claims)
	if err != nil {
		return "", fmt.Errorf("encode token claims: %w", err)
	}

	unsigned := encodedHeader + "." + encodedClaims
	signature := manager.sign(unsigned)
	return unsigned + "." + signature, nil
}

func (manager *TokenManager) Parse(token string) (Principal, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return Principal{}, errors.New("invalid token")
	}

	unsigned := parts[0] + "." + parts[1]
	expectedSignature := manager.sign(unsigned)
	if !hmac.Equal([]byte(parts[2]), []byte(expectedSignature)) {
		return Principal{}, errors.New("invalid token signature")
	}

	var header tokenHeader
	if err := decodeSegment(parts[0], &header); err != nil {
		return Principal{}, errors.New("invalid token header")
	}
	if header.Algorithm != "HS256" || header.Type != "JWT" {
		return Principal{}, errors.New("unsupported token")
	}

	var claims tokenClaims
	if err := decodeSegment(parts[1], &claims); err != nil {
		return Principal{}, errors.New("invalid token claims")
	}
	if claims.Issuer != manager.issuer || claims.Subject == "" {
		return Principal{}, errors.New("invalid token claims")
	}
	if claims.Role != RoleUser && claims.Role != RoleAdmin {
		return Principal{}, errors.New("invalid token role")
	}
	if time.Now().UTC().Unix() >= claims.ExpiresAt {
		return Principal{}, errors.New("token expired")
	}

	return Principal{UserID: claims.Subject, Role: claims.Role}, nil
}

func (manager *TokenManager) LifetimeSeconds() int64 {
	return int64(manager.ttl.Seconds())
}

func (manager *TokenManager) sign(value string) string {
	mac := hmac.New(sha256.New, manager.secret)
	_, _ = mac.Write([]byte(value))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func encodeSegment(value any) (string, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(encoded), nil
}

func decodeSegment(segment string, destination any) error {
	decoded, err := base64.RawURLEncoding.DecodeString(segment)
	if err != nil {
		return err
	}
	return json.Unmarshal(decoded, destination)
}
