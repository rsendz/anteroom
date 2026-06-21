// Package token issues and verifies the HMAC-signed cookie that identifies a
// visitor across requests. The token carries only routing hints: Redis is
// always the authority on whether a visitor is actually admitted, so a
// replayed or hand-crafted token can never grant access on its own — it can
// at most name a visitor ID, and forging one requires the secret.
package token

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"strings"
	"time"
)

// CookieName is the cookie anteroom sets on every visitor.
const CookieName = "ar_visitor"

// MaxAge bounds how long a signed token is honoured. An older token is
// treated as absent, so the visitor simply rejoins the queue.
const MaxAge = 24 * time.Hour

// Status is the visitor's last known state. It is a hint that saves a Redis
// lookup on the common path, never an authorization claim.
type Status string

const (
	StatusWaiting  Status = "w"
	StatusAdmitted Status = "a"
)

// Payload is the signed body of the cookie.
type Payload struct {
	ID     string `json:"id"`
	Room   string `json:"room"`
	Status Status `json:"st"`
	// IssuedAt is unix seconds.
	IssuedAt int64 `json:"iat"`
}

// NewID returns a fresh random visitor ID.
func NewID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("anteroom: crypto/rand unavailable: " + err.Error())
	}
	return hex.EncodeToString(b[:])
}

// Signer signs and verifies payloads with a shared secret.
type Signer struct {
	secret []byte
	now    func() time.Time
}

func New(secret string) *Signer {
	return &Signer{secret: []byte(secret), now: time.Now}
}

// NewWithClock is used by tests to control token age.
func NewWithClock(secret string, now func() time.Time) *Signer {
	return &Signer{secret: []byte(secret), now: now}
}

var enc = base64.RawURLEncoding

// Sign returns the cookie value for p, stamping IssuedAt if unset.
func (s *Signer) Sign(p Payload) string {
	if p.IssuedAt == 0 {
		p.IssuedAt = s.now().Unix()
	}
	body, err := json.Marshal(p)
	if err != nil {
		// Payload is a fixed struct of strings and an int; marshalling it
		// cannot fail.
		panic("anteroom: marshal token payload: " + err.Error())
	}
	b64 := enc.EncodeToString(body)
	return b64 + "." + enc.EncodeToString(s.mac(b64))
}

// Verify parses and authenticates a cookie value. ok is false for any
// malformed, tampered, or stale token; callers treat that as "no cookie" and
// never as an error worth showing the visitor.
func (s *Signer) Verify(value string) (p Payload, ok bool) {
	b64, sig, found := strings.Cut(value, ".")
	if !found {
		return Payload{}, false
	}
	gotMAC, err := enc.DecodeString(sig)
	if err != nil {
		return Payload{}, false
	}
	if !hmac.Equal(gotMAC, s.mac(b64)) {
		return Payload{}, false
	}
	body, err := enc.DecodeString(b64)
	if err != nil {
		return Payload{}, false
	}
	if err := json.Unmarshal(body, &p); err != nil {
		return Payload{}, false
	}
	if p.ID == "" || p.Room == "" {
		return Payload{}, false
	}
	if age := s.now().Sub(time.Unix(p.IssuedAt, 0)); age > MaxAge || age < -MaxAge {
		return Payload{}, false
	}
	return p, true
}

func (s *Signer) mac(b64 string) []byte {
	m := hmac.New(sha256.New, s.secret)
	m.Write([]byte(b64))
	return m.Sum(nil)
}
