package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const validYAML = `
listen: ":9090"
cookie_secret: "0123456789abcdef"
admin_token: "secret"
admit_interval: 100ms
redis:
  addr: "redis:6379"
kafka:
  brokers: ["kafka:9092"]
  topic: "events"
rooms:
  shop:
    match_host: shop.example.com
    origin: http://shop:3000
    rate: 50
    max_active: 100
    session_ttl: 2m
    abandon_after: 30s
    title: "The Shop"
  fallback:
    origin: http://fallback:3000
`

func writeConfig(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "anteroom.yaml")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadValid(t *testing.T) {
	cfg, err := Load(writeConfig(t, validYAML))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Listen != ":9090" {
		t.Errorf("Listen = %q", cfg.Listen)
	}
	if cfg.AdmitInterval.Std() != 100*time.Millisecond {
		t.Errorf("AdmitInterval = %v", cfg.AdmitInterval.Std())
	}
	shop := cfg.Rooms["shop"]
	if shop.Rate != 50 || shop.MaxActive != 100 || shop.SessionTTL.Std() != 2*time.Minute {
		t.Errorf("shop room = %+v", shop)
	}
	if shop.Title != "The Shop" {
		t.Errorf("shop title = %q", shop.Title)
	}
	// Defaults fill the sparse fallback room.
	fb := cfg.Rooms["fallback"]
	if fb.Rate != DefaultRate || fb.MaxActive != DefaultMaxActive ||
		fb.SessionTTL.Std() != DefaultSessionTTL || fb.AbandonAfter.Std() != DefaultAbandonAfter {
		t.Errorf("fallback defaults not applied: %+v", fb)
	}
	if fb.Title != "fallback" {
		t.Errorf("fallback title = %q, want room name", fb.Title)
	}
}

func TestLoadDefaults(t *testing.T) {
	minimal := `
cookie_secret: "0123456789abcdef"
admin_token: "secret"
rooms:
  main:
    origin: http://localhost:3000
`
	cfg, err := Load(writeConfig(t, minimal))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Listen != ":8080" || cfg.Redis.Addr != "localhost:6379" ||
		cfg.Kafka.Topic != "anteroom.events" || cfg.AdmitInterval.Std() != 250*time.Millisecond {
		t.Errorf("defaults not applied: %+v", cfg)
	}
	if len(cfg.Kafka.Brokers) != 0 {
		t.Errorf("brokers should default empty, got %v", cfg.Kafka.Brokers)
	}
}

func TestEnvOverrides(t *testing.T) {
	t.Setenv("ANTEROOM_LISTEN", ":7777")
	t.Setenv("ANTEROOM_REDIS_ADDR", "envredis:6379")
	t.Setenv("ANTEROOM_KAFKA_BROKERS", "b1:9092, b2:9092")
	t.Setenv("ANTEROOM_SECURE_COOKIES", "true")
	t.Setenv("ANTEROOM_ADMIT_INTERVAL", "1s")
	cfg, err := Load(writeConfig(t, validYAML))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Listen != ":7777" || cfg.Redis.Addr != "envredis:6379" || !cfg.SecureCookies {
		t.Errorf("env overrides not applied: %+v", cfg)
	}
	if len(cfg.Kafka.Brokers) != 2 || cfg.Kafka.Brokers[1] != "b2:9092" {
		t.Errorf("brokers = %v", cfg.Kafka.Brokers)
	}
	if cfg.AdmitInterval.Std() != time.Second {
		t.Errorf("AdmitInterval = %v", cfg.AdmitInterval.Std())
	}
}

func TestEnvOverrideInvalid(t *testing.T) {
	t.Setenv("ANTEROOM_SECURE_COOKIES", "not-a-bool")
	if _, err := Load(writeConfig(t, validYAML)); err == nil ||
		!strings.Contains(err.Error(), "ANTEROOM_SECURE_COOKIES") {
		t.Errorf("want env error, got %v", err)
	}
}

func TestValidationErrors(t *testing.T) {
	base := func(mutate string) string {
		return `
cookie_secret: "0123456789abcdef"
admin_token: "secret"
` + mutate
	}
	cases := []struct {
		name, yaml, wantErr string
	}{
		{"short secret", `
cookie_secret: "short"
admin_token: "secret"
rooms:
  a: {origin: http://x:1}
`, "cookie_secret"},
		{"no admin token", `
cookie_secret: "0123456789abcdef"
rooms:
  a: {origin: http://x:1}
`, "admin_token"},
		{"no rooms", base(`rooms: {}`), "at least one room"},
		{"missing origin", base(`
rooms:
  a: {match_host: a.com}
`), "origin is required"},
		{"relative origin", base(`
rooms:
  a: {origin: "not a url"}
`), "absolute URL"},
		{"negative rate", base(`
rooms:
  a: {origin: "http://x:1", rate: -1}
`), "rate must be positive"},
		{"duplicate host", base(`
rooms:
  a: {origin: "http://x:1", match_host: same.com}
  b: {origin: "http://y:1", match_host: same.com}
`), "both match host"},
		{"two catch-alls", base(`
rooms:
  a: {origin: "http://x:1"}
  b: {origin: "http://y:1"}
`), "at most one room may omit match_host"},
		{"unknown field", base(`
rooms:
  a: {origin: "http://x:1"}
typo_field: true
`), "typo_field"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(writeConfig(t, tc.yaml))
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("want error containing %q, got %v", tc.wantErr, err)
			}
		})
	}
}

func TestRoomForHost(t *testing.T) {
	cfg, err := Load(writeConfig(t, validYAML))
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		host, want string
		ok         bool
	}{
		{"shop.example.com", "shop", true},
		{"shop.example.com:8080", "shop", true},
		{"other.example.com", "fallback", true},
		{"", "fallback", true},
	}
	for _, tc := range cases {
		got, ok := cfg.RoomForHost(tc.host)
		if got != tc.want || ok != tc.ok {
			t.Errorf("RoomForHost(%q) = %q,%v; want %q,%v", tc.host, got, ok, tc.want, tc.ok)
		}
	}

	// Without a catch-all, unmatched hosts miss.
	delete(cfg.Rooms, "fallback")
	if name, ok := cfg.RoomForHost("nope.com"); ok {
		t.Errorf("expected no match, got %q", name)
	}
}

func TestMissingFile(t *testing.T) {
	if _, err := Load("/nonexistent/anteroom.yaml"); err == nil {
		t.Error("want error for missing file")
	}
}

func TestScheduleParsing(t *testing.T) {
	cfg, err := Load(writeConfig(t, `
cookie_secret: "0123456789abcdef"
admin_token: "secret"
trusted_proxies: ["10.0.0.0/8"]
rooms:
  drop:
    origin: http://backend:3000
    lottery: true
    schedule:
      queue_opens_at: 2026-11-20T09:30:00Z
      admits_at: 2026-11-20T10:00:00Z
      closes_at: 2026-11-20T12:00:00Z
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	room := cfg.Rooms["drop"]
	if !room.Lottery {
		t.Error("lottery was not parsed")
	}
	if !room.Schedule.IsSet() {
		t.Fatal("schedule was not parsed")
	}
	if got := room.Schedule.AdmitsAt.UTC().Format(time.RFC3339); got != "2026-11-20T10:00:00Z" {
		t.Errorf("admits_at = %s", got)
	}
	if len(cfg.TrustedProxies) != 1 {
		t.Errorf("trusted_proxies = %v", cfg.TrustedProxies)
	}
}

func TestUnscheduledRoomHasNoSchedule(t *testing.T) {
	cfg, err := Load(writeConfig(t, validYAML))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Rooms["shop"].Schedule.IsSet() {
		t.Error("a room with no schedule block reported one")
	}
}

func TestScheduleAndLimitValidation(t *testing.T) {
	base := func(room string) string {
		return `
cookie_secret: "0123456789abcdef"
admin_token: "secret"
rooms:
  drop:
    origin: http://backend:3000
` + room
	}
	cases := []struct{ name, yaml, wantErr string }{
		{"doors before queue", base(`
    schedule:
      queue_opens_at: 2026-11-20T10:00:00Z
      admits_at: 2026-11-20T09:00:00Z
`), "before queue_opens_at"},
		{"closes before admits", base(`
    schedule:
      admits_at: 2026-11-20T10:00:00Z
      closes_at: 2026-11-20T09:00:00Z
`), "closes_at must be after admits_at"},
		{"queue opens with no doors", base(`
    schedule:
      queue_opens_at: 2026-11-20T10:00:00Z
`), "needs admits_at"},
		{"lottery without a schedule", base(`
    lottery: true
`), "lottery needs a schedule"},
		{"unparseable time", base(`
    schedule:
      admits_at: "next tuesday"
`), "RFC 3339"},
		{"negative join limit", base(`
    join_limit_per_ip: -5
`), "cannot be negative"},
		{"bad trusted proxy", `
cookie_secret: "0123456789abcdef"
admin_token: "secret"
trusted_proxies: ["10.0.0.1"]
rooms:
  a: {origin: "http://x:1"}
`, "not a CIDR"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(writeConfig(t, tc.yaml))
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("want error containing %q, got %v", tc.wantErr, err)
			}
		})
	}
}

func TestJoinLimitZeroDisablesRatherThanDefaults(t *testing.T) {
	// An explicit 0 must survive: an operator turning the limit off should not
	// silently get the default back.
	cfg, err := Load(writeConfig(t, `
cookie_secret: "0123456789abcdef"
admin_token: "secret"
rooms:
  off:
    origin: http://backend:3000
    join_limit_per_ip: 0
  unset:
    origin: http://backend:3000
    match_host: unset.test
`))
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Rooms["off"].JoinLimit(); got != 0 {
		t.Errorf("explicit 0 became %d", got)
	}
	if got := cfg.Rooms["unset"].JoinLimit(); got != DefaultJoinLimitPerIP {
		t.Errorf("unset limit = %d, want the default %d", got, DefaultJoinLimitPerIP)
	}
}
