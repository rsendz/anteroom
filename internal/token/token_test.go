package token

import (
	"strings"
	"testing"
	"time"
)

const secret = "0123456789abcdef"

func TestRoundTrip(t *testing.T) {
	s := New(secret)
	want := Payload{ID: NewID(), Room: "shop", Status: StatusWaiting}
	got, ok := s.Verify(s.Sign(want))
	if !ok {
		t.Fatal("Verify failed on freshly signed token")
	}
	if got.ID != want.ID || got.Room != want.Room || got.Status != want.Status {
		t.Errorf("round trip = %+v, want %+v", got, want)
	}
	if got.IssuedAt == 0 {
		t.Error("IssuedAt not stamped")
	}
}

func TestNewIDIsUniqueAndHex(t *testing.T) {
	seen := map[string]bool{}
	for range 100 {
		id := NewID()
		if len(id) != 32 {
			t.Fatalf("id %q length = %d, want 32", id, len(id))
		}
		if seen[id] {
			t.Fatalf("duplicate id %q", id)
		}
		seen[id] = true
	}
}

func TestVerifyRejects(t *testing.T) {
	s := New(secret)
	valid := s.Sign(Payload{ID: "abc", Room: "shop", Status: StatusAdmitted})
	body, sig, _ := strings.Cut(valid, ".")

	cases := []struct {
		name, value string
	}{
		{"empty", ""},
		{"no separator", body},
		{"garbage", "not-a-token"},
		{"tampered payload", enc.EncodeToString([]byte(`{"id":"evil","room":"shop","st":"a"}`)) + "." + sig},
		{"tampered signature", body + "." + enc.EncodeToString([]byte("wrong-signature-bytes-here!!"))},
		{"unpadded sig", body + ".zzz"},
		{"empty payload", enc.EncodeToString([]byte(`{}`)) + "." + sig},
		{"swapped halves", sig + "." + body},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, ok := s.Verify(tc.value); ok {
				t.Errorf("Verify(%q) accepted an invalid token", tc.value)
			}
		})
	}
}

func TestWrongSecretRejected(t *testing.T) {
	signed := New(secret).Sign(Payload{ID: "abc", Room: "shop"})
	if _, ok := New("different-secret!").Verify(signed); ok {
		t.Error("token signed with another secret was accepted")
	}
}

func TestMissingRoomRejected(t *testing.T) {
	// A token without a room can't be scoped to a host, so it is not usable.
	s := New(secret)
	if _, ok := s.Verify(s.Sign(Payload{ID: "abc"})); ok {
		t.Error("token without room was accepted")
	}
}

func TestExpiry(t *testing.T) {
	base := time.Now()
	issuer := NewWithClock(secret, func() time.Time { return base })
	tok := issuer.Sign(Payload{ID: "abc", Room: "shop", Status: StatusWaiting})

	fresh := NewWithClock(secret, func() time.Time { return base.Add(MaxAge - time.Minute) })
	if _, ok := fresh.Verify(tok); !ok {
		t.Error("token just inside MaxAge was rejected")
	}

	stale := NewWithClock(secret, func() time.Time { return base.Add(MaxAge + time.Minute) })
	if _, ok := stale.Verify(tok); ok {
		t.Error("stale token was accepted")
	}

	// Clock skew far into the past is equally suspect.
	past := NewWithClock(secret, func() time.Time { return base.Add(-2 * MaxAge) })
	if _, ok := past.Verify(tok); ok {
		t.Error("far-future token was accepted")
	}
}

func FuzzVerify(f *testing.F) {
	s := New(secret)
	f.Add(s.Sign(Payload{ID: "abc", Room: "shop", Status: StatusWaiting}))
	f.Add("")
	f.Add(".")
	f.Add("a.b")
	f.Fuzz(func(t *testing.T, value string) {
		// Verify must never panic, and must never accept an unsigned value.
		if p, ok := s.Verify(value); ok {
			if value != s.Sign(p) {
				t.Errorf("accepted token %q that does not re-sign to itself", value)
			}
		}
	})
}
