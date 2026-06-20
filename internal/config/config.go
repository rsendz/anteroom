// Package config loads and validates anteroom configuration from a YAML
// file, with flat ANTEROOM_* environment variable overrides.
package config

import (
	"fmt"
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

func (d Duration) MarshalYAML() (any, error) { return time.Duration(d).String(), nil }

func (d Duration) Std() time.Duration { return time.Duration(d) }

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
}

type Redis struct {
	Addr string `yaml:"addr"`
}

type Kafka struct {
	Brokers []string `yaml:"brokers"`
	Topic   string   `yaml:"topic"`
}

type Config struct {
	Listen        string          `yaml:"listen"`
	AdmitInterval Duration        `yaml:"admit_interval"`
	CookieSecret  string          `yaml:"cookie_secret"`
	AdminToken    string          `yaml:"admin_token"`
	SecureCookies bool            `yaml:"secure_cookies"`
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
)

// Default returns a Config with server-level defaults; it has no rooms.
func Default() Config {
	return Config{
		Listen:        ":8080",
		AdmitInterval: Duration(250 * time.Millisecond),
		Redis:         Redis{Addr: "localhost:6379"},
		Kafka:         Kafka{Topic: "anteroom.events"},
		Rooms:         map[string]Room{},
	}
}

// DefaultRoom returns a Room for origin with all defaults applied.
func DefaultRoom(origin string) Room {
	return Room{
		Origin:       origin,
		Rate:         DefaultRate,
		MaxActive:    DefaultMaxActive,
		SessionTTL:   Duration(DefaultSessionTTL),
		AbandonAfter: Duration(DefaultAbandonAfter),
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
