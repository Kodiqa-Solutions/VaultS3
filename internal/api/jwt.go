package api

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// JWTService implements HS256 JWT signing and validation with zero dependencies.
type JWTService struct {
	secret []byte
}

type jwtHeader struct {
	Alg string `json:"alg"`
	Typ string `json:"typ"`
}

type JWTClaims struct {
	Sub string `json:"sub"`
	Iat int64  `json:"iat"`
	Exp int64  `json:"exp"`
}

// NewJWTService signs sessions with key, which must be random and per
// installation.
//
// It used to be derived from the admin secret, `HMAC-SHA256(fixed salt,
// adminSecret)`. That made the signing key a function of a credential: anyone
// who learned the admin secret, including everyone who read the default config
// that used to ship one, could mint a `sub=admin` token offline and never touch
// the login endpoint. The key is now independent of every credential, so
// learning a password no longer yields the ability to forge sessions.
//
// An empty key is refused rather than silently accepted, since a zero-length
// HMAC key would be trivially guessable.
func NewJWTService(key []byte) *JWTService {
	if len(key) == 0 {
		// Better an unusable service than a predictable one. Callers construct
		// this from a persisted random key; a bug that loses it must not
		// downgrade to a guessable signer.
		key = mustRandomKey()
	}
	return &JWTService{secret: append([]byte(nil), key...)}
}

// NewRandomJWTKey returns a fresh 32-byte signing key.
func NewRandomJWTKey() ([]byte, error) {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("generate jwt signing key: %w", err)
	}
	return key, nil
}

func mustRandomKey() []byte {
	key, err := NewRandomJWTKey()
	if err != nil {
		// crypto/rand failing is unrecoverable; a predictable fallback would be
		// worse than refusing to run.
		panic("vaults3: cannot generate a JWT signing key: " + err.Error())
	}
	return key
}

func (j *JWTService) Generate(subject string, ttl time.Duration) (string, error) {
	header := jwtHeader{Alg: "HS256", Typ: "JWT"}
	claims := JWTClaims{
		Sub: subject,
		Iat: time.Now().Unix(),
		Exp: time.Now().Add(ttl).Unix(),
	}

	headerJSON, err := json.Marshal(header)
	if err != nil {
		return "", err
	}
	claimsJSON, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}

	headerB64 := base64.RawURLEncoding.EncodeToString(headerJSON)
	claimsB64 := base64.RawURLEncoding.EncodeToString(claimsJSON)

	signingInput := headerB64 + "." + claimsB64
	sig := j.sign([]byte(signingInput))
	sigB64 := base64.RawURLEncoding.EncodeToString(sig)

	return signingInput + "." + sigB64, nil
}

func (j *JWTService) Validate(tokenStr string) (*JWTClaims, error) {
	parts := strings.SplitN(tokenStr, ".", 3)
	if len(parts) != 3 {
		return nil, fmt.Errorf("invalid token format")
	}

	// Verify signature
	signingInput := parts[0] + "." + parts[1]
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return nil, fmt.Errorf("invalid signature encoding")
	}

	expected := j.sign([]byte(signingInput))
	if !hmac.Equal(sig, expected) {
		return nil, fmt.Errorf("invalid signature")
	}

	// Decode claims
	claimsJSON, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, fmt.Errorf("invalid claims encoding")
	}

	var claims JWTClaims
	if err := json.Unmarshal(claimsJSON, &claims); err != nil {
		return nil, fmt.Errorf("invalid claims: %w", err)
	}

	// Check expiry
	if time.Now().Unix() > claims.Exp {
		return nil, fmt.Errorf("token expired")
	}

	return &claims, nil
}

func (j *JWTService) sign(data []byte) []byte {
	h := hmac.New(sha256.New, j.secret)
	h.Write(data)
	return h.Sum(nil)
}
