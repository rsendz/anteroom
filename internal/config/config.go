// Package config loads and validates anteroom configuration from a YAML
// file, with flat ANTEROOM_* environment variable overrides.
package config

import (
	"fmt"
	"net/netip"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Duration wraps time.Duration so YAML values like "5m" or "250ms" parse.
type Duration time.Duration

func (d *Duration) UnmarshalYAML(node *yaml.Node) error {
	var s string
	if err := node.Decode(&s); err != nil {
		return err
	}
	parsed, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", s, err)
	}
	*d = Duration(parsed)
	return nil
}

func (d Duration) Std() time.Duration { return time.Duration(d) }

// Timestamp is an RFC 3339 instant, e.g. 2026-11-20T10:00:00Z. A zero value
// means the field was not set.
type Timestamp struct{ time.Time }

func (t *Timestamp) UnmarshalYAML(node *yaml.Node) error {
	var s string
	if err := node.Decode(&s); err != nil {
		return err
	}
	if strings.TrimSpace(s) == "" {
		return nil
	}
	parsed, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return fmt.Errorf("invalid time %q: want RFC 3339 such as 2026-11-20T10:00:00Z", s)
	}
	t.Time = parsed
	return nil
}

// Schedule opens a room at a fixed time, which is what a ticket sale or a
// product drop needs. Visitors may line up from QueueOpensAt, nobody is
// admitted until AdmitsAt, and no new admissions happen after ClosesAt.
type Schedule struct {
	QueueOpensAt Timestamp `yaml:"queue_opens_at"`
	AdmitsAt     Timestamp `yaml:"admits_at"`
	ClosesAt     Timestamp `yaml:"closes_at"`
}

// IsSet reports whether this room has any scheduled window at all.
func (s Schedule) IsSet() bool {
	return !s.QueueOpensAt.IsZero() || !s.AdmitsAt.IsZero() || !s.ClosesAt.IsZero()
}

func (s Schedule) validate(room string) error {
	opens, admits, closes := s.QueueOpensAt, s.AdmitsAt, s.ClosesAt
	if !opens.IsZero() && !admits.IsZero() && admits.Before(opens.Time) {
		return fmt.Errorf("room %q: schedule.admits_at is before queue_opens_at, so the doors would open before the queue does", room)
	}
	if !closes.IsZero() && !admits.IsZero() && !closes.After(admits.Time) {
		return fmt.Errorf("room %q: schedule.closes_at must be after admits_at", room)
	}
	if !closes.IsZero() && !opens.IsZero() && !closes.After(opens.Time) {
		return fmt.Errorf("room %q: schedule.closes_at must be after queue_opens_at", room)
	}
	if !opens.IsZero() && admits.IsZero() {
		return fmt.Errorf("room %q: schedule.queue_opens_at needs admits_at, or the queue would never open", room)
	}
	return nil
}

// Room configures one waiting room: which host it matches, where admitted
// traffic goes, and its admission limits. A room with an empty MatchHost is
// the catch-all; at most one is allowed.
type Room struct {
	MatchHost    string   `yaml:"match_host"`
	Origin       string   `yaml:"origin"`
	Rate         float64  `yaml:"rate"`
	MaxActive    int      `yaml:"max_active"`
	SessionTTL   Duration `yaml:"session_ttl"`
	AbandonAfter Duration `yaml:"abandon_after"`
	Title        string   `yaml:"title"`
	Message      string   `yaml:"message"`
	// PreserveHost forwards the visitor's Host header to the origin instead of
	// the origin's own. Needed when the backend serves several virtual hosts;
	// wrong when the origin is an external site that expects its own name.
	PreserveHost bool `yaml:"preserve_host"`
	// JoinLimitPerIP caps how many visitors may newly enter the queue from one
	// address per JoinLimitWindow, which is what stops a script taking
	// thousands of places. It is a pointer so that an explicit 0 (disable the
	// limit) is distinguishable from the field being absent (use the default).
	// After ApplyEnvAndDefaults it is never nil; read it through JoinLimit.
	JoinLimitPerIP  *int     `yaml:"join_limit_per_ip"`
	JoinLimitWindow Duration `yaml:"join_limit_window"`

	// Schedule opens the room at a fixed time rather than immediately.
	Schedule Schedule `yaml:"schedule"`
	// Lottery settles the order of everyone who lined up before the doors open
	// by drawing rather than by arrival, so turning up early gains nothing.
	// It has no effect without a schedule.
	Lottery bool `yaml:"lottery"`
}

// JoinLimit is the resolved per-address join limit; 0 means no limit.
func (r Room) JoinLimit() int {
	if r.JoinLimitPerIP == nil {
		return DefaultJoinLimitPerIP
	}
	return *r.JoinLimitPerIP
}

type Redis struct {
	Addr string `yaml:"addr"`
}

type Kafka struct {
	Brokers []string `yaml:"brokers"`
	Topic   string   `yaml:"topic"`
}

type Config struct {
	Listen        string   `yaml:"listen"`
	AdmitInterval Duration `yaml:"admit_interval"`
	CookieSecret  string   `yaml:"cookie_secret"`
	AdminToken    string   `yaml:"admin_token"`
	SecureCookies bool     `yaml:"secure_cookies"`
	// TrustedProxies lists the CIDRs anteroom sits behind. X-Forwarded-For is
	// honoured only on requests arriving from these, because the header is
	// otherwise trivially forged and per-address limits would mean nothing.
	TrustedProxies []string `yaml:"trusted_proxies"`
	// FailOpen lets visitors straight through when the queue store has been
	// unreachable for FailOpenAfter, trading the origin's protection for the
	// site staying up. Off by default: failing open silently is how a waiting
	// room stops being one.
	FailOpen      bool            `yaml:"fail_open"`
	FailOpenAfter Duration        `yaml:"fail_open_after"`
	Redis         Redis           `yaml:"redis"`
	Kafka         Kafka           `yaml:"kafka"`
	Rooms         map[string]Room `yaml:"rooms"`
}

// Room defaults applied to zero-valued fields.
const (
	DefaultRate         = 5.0
	DefaultMaxActive    = 500
	DefaultSessionTTL   = 5 * time.Minute
	DefaultAbandonAfter = 60 * time.Second

	// DefaultJoinLimitPerIP is deliberately loose. Office NAT and mobile
	// carrier-grade NAT put a great many legitimate visitors behind a single
	// address, so the default is set to stop a script taking thousands of
	// places without turning away a crowd arriving from one gateway. The
	// refused counter on the room snapshot is the signal to raise it.
	DefaultJoinLimitPerIP  = 120
	DefaultJoinLimitWindow = time.Minute

	// DefaultFailOpenAfter is long enough that a blip or a failover does not
	// release the queue, short enough that a real outage does not keep a site
	// dark for long.
	DefaultFailOpenAfter = 30 * time.Second
)

// Default returns a Config with server-level defaults; it has no rooms.
func Default() Config {
	return Config{
		Listen:        ":8080",
		AdmitInterval: Duration(250 * time.Millisecond),
		Redis:         Redis{Addr: "localhost:6379"},
		Kafka:         Kafka{Topic: "anteroom.events"},
		FailOpenAfter: Duration(DefaultFailOpenAfter),
		Rooms:         map[string]Room{},
	}
}

// DefaultRoom returns a Room for origin with all defaults applied.
func DefaultRoom(origin string) Room {
	return Room{
		Origin:          origin,
		Rate:            DefaultRate,
		MaxActive:       DefaultMaxActive,
		SessionTTL:      Duration(DefaultSessionTTL),
		AbandonAfter:    Duration(DefaultAbandonAfter),
		JoinLimitWindow: Duration(DefaultJoinLimitWindow),
	}
}

// Load reads path (if non-empty), applies ANTEROOM_* env overrides and room
// defaults, and validates. Callers wanting a config-less setup pass an empty
// path and add rooms before calling Validate themselves.
func Load(path string) (Config, error) {
	cfg := Default()
	if path != "" {
		raw, err := os.ReadFile(path)
		if err != nil {
			return cfg, fmt.Errorf("read config: %w", err)
		}
		dec := yaml.NewDecoder(strings.NewReader(string(raw)))
		dec.KnownFields(true)
		if err := dec.Decode(&cfg); err != nil {
			return cfg, fmt.Errorf("parse %s: %w", path, err)
		}
	}
	if err := cfg.ApplyEnvAndDefaults(); err != nil {
		return cfg, err
	}
	if err := cfg.Validate(); err != nil {
		return cfg, err
	}
	return cfg, nil
}

// ApplyEnvAndDefaults layers ANTEROOM_* overrides over the config and fills in
// per-room defaults. Load does this for you; callers building a Config in code
// (such as single-room mode) call it themselves before Validate.
func (c *Config) ApplyEnvAndDefaults() error {
	if err := c.applyEnv(); err != nil {
		return err
	}
	if c.FailOpenAfter <= 0 {
		c.FailOpenAfter = Duration(DefaultFailOpenAfter)
	}
	c.applyRoomDefaults()
	return nil
}

func (c *Config) applyEnv() error {
	var firstErr error
	set := func(key string, fn func(string) error) {
		if v, ok := os.LookupEnv("ANTEROOM_" + key); ok {
			if err := fn(v); err != nil && firstErr == nil {
				firstErr = fmt.Errorf("env ANTEROOM_%s: %w", key, err)
			}
		}
	}
	str := func(dst *string) func(string) error {
		return func(v string) error { *dst = v; return nil }
	}
	set("LISTEN", str(&c.Listen))
	set("COOKIE_SECRET", str(&c.CookieSecret))
	set("ADMIN_TOKEN", str(&c.AdminToken))
	set("REDIS_ADDR", str(&c.Redis.Addr))
	set("KAFKA_TOPIC", str(&c.Kafka.Topic))
	set("KAFKA_BROKERS", func(v string) error {
		c.Kafka.Brokers = nil
		for _, b := range strings.Split(v, ",") {
			if b = strings.TrimSpace(b); b != "" {
				c.Kafka.Brokers = append(c.Kafka.Brokers, b)
			}
		}
		return nil
	})
	set("SECURE_COOKIES", func(v string) error {
		b, err := strconv.ParseBool(v)
		c.SecureCookies = b
		return err
	})
	set("FAIL_OPEN", func(v string) error {
		b, err := strconv.ParseBool(v)
		c.FailOpen = b
		return err
	})
	set("FAIL_OPEN_AFTER", func(v string) error {
		d, err := time.ParseDuration(v)
		c.FailOpenAfter = Duration(d)
		return err
	})
	set("ADMIT_INTERVAL", func(v string) error {
		d, err := time.ParseDuration(v)
		c.AdmitInterval = Duration(d)
		return err
	})
	return firstErr
}

func (c *Config) applyRoomDefaults() {
	for name, r := range c.Rooms {
		if r.Rate == 0 {
			r.Rate = DefaultRate
		}
		if r.MaxActive == 0 {
			r.MaxActive = DefaultMaxActive
		}
		if r.SessionTTL == 0 {
			r.SessionTTL = Duration(DefaultSessionTTL)
		}
		if r.AbandonAfter == 0 {
			r.AbandonAfter = Duration(DefaultAbandonAfter)
		}
		if r.Title == "" {
			r.Title = name
		}
		if r.JoinLimitPerIP == nil {
			limit := DefaultJoinLimitPerIP
			r.JoinLimitPerIP = &limit
		}
		if r.JoinLimitWindow == 0 {
			r.JoinLimitWindow = Duration(DefaultJoinLimitWindow)
		}
		c.Rooms[name] = r
	}
}

func (c *Config) Validate() error {
	if len(c.CookieSecret) < 16 {
		return fmt.Errorf("cookie_secret must be at least 16 bytes (got %d)", len(c.CookieSecret))
	}
	if c.AdminToken == "" {
		return fmt.Errorf("admin_token is required")
	}
	if len(c.Rooms) == 0 {
		return fmt.Errorf("at least one room is required")
	}
	if c.AdmitInterval.Std() <= 0 {
		return fmt.Errorf("admit_interval must be positive")
	}
	for _, cidr := range c.TrustedProxies {
		if _, err := netip.ParsePrefix(strings.TrimSpace(cidr)); err != nil {
			return fmt.Errorf("trusted_proxies: %q is not a CIDR block such as 10.0.0.0/8: %w", cidr, err)
		}
	}
	catchAlls, hosts := 0, map[string]string{}
	for name, r := range c.Rooms {
		if !validRoomName(name) {
			return fmt.Errorf("room %q: name must be 1-64 characters of letters, digits, '-' or '_'", name)
		}
		if r.Origin == "" {
			return fmt.Errorf("room %q: origin is required", name)
		}
		u, err := url.Parse(r.Origin)
		if err != nil || u.Scheme == "" || u.Host == "" {
			return fmt.Errorf("room %q: origin %q must be an absolute URL", name, r.Origin)
		}
		if r.Rate <= 0 {
			return fmt.Errorf("room %q: rate must be positive", name)
		}
		if r.MaxActive <= 0 {
			return fmt.Errorf("room %q: max_active must be positive", name)
		}
		if r.SessionTTL.Std() <= 0 || r.AbandonAfter.Std() <= 0 {
			return fmt.Errorf("room %q: session_ttl and abandon_after must be positive", name)
		}
		if r.JoinLimit() < 0 {
			return fmt.Errorf("room %q: join_limit_per_ip cannot be negative (use 0 to disable)", name)
		}
		if r.JoinLimitWindow.Std() <= 0 {
			return fmt.Errorf("room %q: join_limit_window must be positive", name)
		}
		if err := r.Schedule.validate(name); err != nil {
			return err
		}
		if r.Lottery && !r.Schedule.IsSet() {
			return fmt.Errorf("room %q: lottery needs a schedule; without one there is no window to draw from", name)
		}
		if r.MatchHost == "" {
			catchAlls++
			continue
		}
		if other, dup := hosts[r.MatchHost]; dup {
			return fmt.Errorf("rooms %q and %q both match host %q", other, name, r.MatchHost)
		}
		hosts[r.MatchHost] = name
	}
	if catchAlls > 1 {
		return fmt.Errorf("at most one room may omit match_host (the catch-all); found %d", catchAlls)
	}
	return nil
}

// validRoomName keeps room names safe to embed in Redis keys and URLs.
func validRoomName(name string) bool {
	if name == "" || len(name) > 64 {
		return false
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
		default:
			return false
		}
	}
	return true
}

// RoomForHost resolves a request Host header (port stripped by the caller or
// not — both accepted) to a room name, falling back to the catch-all.
// ok is false when no room matches.
func (c *Config) RoomForHost(host string) (name string, ok bool) {
	if i := strings.LastIndex(host, ":"); i > 0 && !strings.Contains(host[i:], "]") {
		host = host[:i]
	}
	catchAll := ""
	for n, r := range c.Rooms {
		if r.MatchHost == host {
			return n, true
		}
		if r.MatchHost == "" {
			catchAll = n
		}
	}
	if catchAll != "" {
		return catchAll, true
	}
	return "", false
}
