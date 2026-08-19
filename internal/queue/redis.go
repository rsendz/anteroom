package queue

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
)

// KeyPrefix namespaces every key anteroom owns, so it can share a Redis
// instance with other applications.
const KeyPrefix = "ar"

// RedisStore implements Store on top of Redis.
type RedisStore struct {
	rdb redis.Scripter
	// now is injectable so tests can advance time without sleeping.
	now func() time.Time
}

// NewRedisStore wraps a Redis client. The client is not closed by the store.
func NewRedisStore(rdb redis.Scripter) *RedisStore {
	return &RedisStore{rdb: rdb, now: time.Now}
}

// SetClock replaces the store's clock. Intended for tests.
func (s *RedisStore) SetClock(now func() time.Time) { s.now = now }

var _ Store = (*RedisStore)(nil)

func key(room, suffix string) string { return KeyPrefix + ":" + room + ":" + suffix }

func keysFor(room string, suffixes ...string) []string {
	out := make([]string, len(suffixes))
	for i, suffix := range suffixes {
		out[i] = key(room, suffix)
	}
	return out
}

func (s *RedisStore) Join(ctx context.Context, room, id string) (int64, error) {
	pos, err := joinScript.Run(ctx, s.rdb,
		keysFor(room, "seq", "waiting", "seen", "stats"),
		id, unixSeconds(s.now()),
	).Int64()
	if err != nil {
		return 0, fmt.Errorf("queue: join %s/%s: %w", room, id, err)
	}
	return pos, nil
}

func (s *RedisStore) Resolve(ctx context.Context, room, id, bucket string) (Resolution, error) {
	// The limit itself lives in the room's settings and is read inside the
	// script, so a change through the admin API takes effect at once and the
	// request path stays a single round trip. Without a bucket there is nobody
	// to count against, so the limit is skipped rather than lumping every such
	// visitor into one shared budget.
	applies := "0"
	joinKey := key(room, "joinbudget")
	if bucket != "" {
		applies = "1"
		joinKey = key(room, "join:"+bucket)
	}

	keys := append(keysFor(room, "waiting", "seen", "active", "conf", "seq", "stats"), joinKey)
	raw, err := resolveScript.Run(ctx, s.rdb, keys,
		id, unixMillis(s.now()), applies,
	).Int64Slice()
	if err != nil {
		return Resolution{}, fmt.Errorf("queue: resolve %s/%s: %w", room, id, err)
	}
	if len(raw) != 5 {
		return Resolution{}, fmt.Errorf("queue: resolve %s: reply had %d parts, want 5", room, len(raw))
	}
	return Resolution{
		Admitted: raw[0] == 1,
		Position: raw[1],
		Joined:   raw[2] == 1,
		Refused:  raw[3] == 1,
		Phase:    Phase(raw[4]),
	}, nil
}

func (s *RedisStore) Position(ctx context.Context, room, id string) (int64, error) {
	// Read-only, which keeps serving the waiting page cheap under a spike.
	pos, err := positionScript.Run(ctx, s.rdb, keysFor(room, "waiting"), id).Int64()
	if err != nil {
		return 0, fmt.Errorf("queue: position %s/%s: %w", room, id, err)
	}
	return pos, nil
}

func (s *RedisStore) Heartbeat(ctx context.Context, room, id string) error {
	// XX updates only members already present, and a visitor sits in `seen`
	// exactly while they are waiting, so this can never resurrect anyone.
	err := heartbeatScript.Run(ctx, s.rdb,
		keysFor(room, "seen"), id, unixSeconds(s.now()),
	).Err()
	if err != nil {
		return fmt.Errorf("queue: heartbeat %s/%s: %w", room, id, err)
	}
	return nil
}

func (s *RedisStore) Touch(ctx context.Context, room, id string) (bool, error) {
	alive, err := touchScript.Run(ctx, s.rdb,
		keysFor(room, "active", "conf"),
		id, unixMillis(s.now()),
	).Int64()
	if err != nil {
		return false, fmt.Errorf("queue: touch %s/%s: %w", room, id, err)
	}
	return alive == 1, nil
}

func (s *RedisStore) Admit(ctx context.Context, room string) (AdmitResult, error) {
	raw, err := admitScript.Run(ctx, s.rdb,
		keysFor(room, "waiting", "seen", "active", "bucket", "conf", "stats"),
		unixMillis(s.now()),
	).Slice()
	if err != nil {
		return AdmitResult{}, fmt.Errorf("queue: admit %s: %w", room, err)
	}
	if len(raw) != 3 {
		return AdmitResult{}, fmt.Errorf("queue: admit %s: reply had %d parts, want 3", room, len(raw))
	}
	return AdmitResult{
		Abandoned: toStrings(raw[0]),
		Expired:   toStrings(raw[1]),
		Admitted:  toStrings(raw[2]),
	}, nil
}

func (s *RedisStore) Snapshot(ctx context.Context, room string) (Snapshot, error) {
	now := s.now()
	raw, err := snapshotScript.Run(ctx, s.rdb,
		keysFor(room, "waiting", "active", "conf", "stats"),
		unixMillis(now),
	).Slice()
	if err != nil {
		return Snapshot{}, fmt.Errorf("queue: snapshot %s: %w", room, err)
	}
	if len(raw)%2 != 0 {
		return Snapshot{}, fmt.Errorf("queue: snapshot %s: reply had %d parts, want field/value pairs", room, len(raw))
	}
	// Field/value pairs rather than a fixed order, so adding a number to the
	// script cannot silently shift the meaning of the others.
	num := make(map[string]float64, len(raw)/2)
	for i := 0; i+1 < len(raw); i += 2 {
		field, _ := raw[i].(string)
		value, _ := raw[i+1].(string)
		num[field], _ = strconv.ParseFloat(value, 64)
	}
	return Snapshot{
		Room:             room,
		Waiting:          int64(num["waiting"]),
		Active:           int64(num["active"]),
		Rate:             num["rate"],
		MaxActive:        int(num["cap"]),
		SessionTTLSecs:   int64(num["ttl"]),
		AbandonAfterSecs: int64(num["abandon"]),
		Paused:           num["paused"] == 1,
		JoinLimit:        int(num["join_limit"]),
		JoinWindowSecs:   int64(num["join_window"]),
		TotalJoined:      int64(num["joined"]),
		TotalAdmitted:    int64(num["admitted"]),
		TotalExpired:     int64(num["expired"]),
		TotalAbandoned:   int64(num["abandoned"]),
		TotalRefused:     int64(num["refused"]),
		Phase:            Phase(int(num["phase"])),
		Lottery:          num["lottery"] == 1,
		QueueOpensAtMS:   int64(num["opens_ms"]),
		AdmitsAtMS:       int64(num["admits_ms"]),
		ClosesAtMS:       int64(num["closes_ms"]),
		NowMS:            now.UnixMilli(),
	}, nil
}

func (s *RedisStore) SetConfig(ctx context.Context, room string, cfg RoomConfig) error {
	err := hsetScript.Run(ctx, s.rdb, keysFor(room, "conf"), confFields(cfg)...).Err()
	if err != nil {
		return fmt.Errorf("queue: set config %s: %w", room, err)
	}
	return nil
}

func (s *RedisStore) SetPaused(ctx context.Context, room string, paused bool) error {
	err := hsetScript.Run(ctx, s.rdb, keysFor(room, "conf"), "paused", boolField(paused)).Err()
	if err != nil {
		return fmt.Errorf("queue: set paused %s: %w", room, err)
	}
	return nil
}

func (s *RedisStore) Flush(ctx context.Context, room string) (int64, error) {
	removed, err := flushScript.Run(ctx, s.rdb, keysFor(room, "waiting", "seen")).Int64()
	if err != nil {
		return 0, fmt.Errorf("queue: flush %s: %w", room, err)
	}
	return removed, nil
}

func (s *RedisStore) Seed(ctx context.Context, room string, cfg RoomConfig, overwrite bool) error {
	// Seeding field by field (rather than whole-hash) means a field added in a
	// later version is filled in on upgrade without clobbering live tuning.
	script := seedScript
	if overwrite {
		script = hsetScript
	}
	fields := append(confFields(cfg), "paused", boolField(cfg.Paused))
	if err := script.Run(ctx, s.rdb, keysFor(room, "conf"), fields...).Err(); err != nil {
		return fmt.Errorf("queue: seed %s: %w", room, err)
	}

	// The draw salt is never overwritten, not even by an explicit reseed:
	// changing it would redraw everyone already collected and move people who
	// are watching their place on screen.
	if cfg.DrawSalt != "" {
		err := seedScript.Run(ctx, s.rdb, keysFor(room, "conf"), "draw_salt", cfg.DrawSalt).Err()
		if err != nil {
			return fmt.Errorf("queue: seed salt %s: %w", room, err)
		}
	}
	err := anchorBucketScript.Run(ctx, s.rdb, keysFor(room, "bucket"), unixMillis(s.now())).Err()
	if err != nil {
		return fmt.Errorf("queue: seed bucket %s: %w", room, err)
	}
	return nil
}

func confFields(cfg RoomConfig) []any {
	return []any{
		"rate", strconv.FormatFloat(cfg.Rate, 'f', -1, 64),
		"cap", strconv.Itoa(cfg.MaxActive),
		"ttl_secs", strconv.FormatInt(int64(cfg.SessionTTL.Seconds()), 10),
		"abandon_secs", strconv.FormatInt(int64(cfg.AbandonAfter.Seconds()), 10),
		"join_limit", strconv.Itoa(cfg.JoinLimit),
		"join_window_secs", strconv.FormatInt(int64(cfg.JoinWindow.Seconds()), 10),
		"lottery", boolField(cfg.Lottery),
		"opens_ms", millisField(cfg.QueueOpensAt),
		"admits_ms", millisField(cfg.AdmitsAt),
		"closes_ms", millisField(cfg.ClosesAt),
	}
}

// millisField renders a schedule time, with the zero time meaning "unset".
func millisField(t time.Time) string {
	if t.IsZero() {
		return "0"
	}
	return strconv.FormatInt(t.UnixMilli(), 10)
}

func boolField(b bool) string {
	if b {
		return "1"
	}
	return "0"
}

// unixSeconds keeps millisecond precision so heartbeat and session scores stay
// comparable with the millisecond clock the admission pass uses.
func unixSeconds(t time.Time) string {
	return strconv.FormatFloat(float64(t.UnixMilli())/1000, 'f', 3, 64)
}

func unixMillis(t time.Time) string { return strconv.FormatInt(t.UnixMilli(), 10) }

func toStrings(v any) []string {
	items, ok := v.([]any)
	if !ok || len(items) == 0 {
		return nil
	}
	out := make([]string, 0, len(items))
	for _, item := range items {
		if s, ok := item.(string); ok {
			out = append(out, s)
		}
	}
	return out
}
