// Command anteroom puts a fair waiting room in front of a website.
//
// With a config file it can protect several sites at once; with a single
// --origin flag it protects one, no configuration required.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/luisresendez/anteroom/internal/admit"
	"github.com/luisresendez/anteroom/internal/config"
	"github.com/luisresendez/anteroom/internal/events"
	"github.com/luisresendez/anteroom/internal/httpserver"
	"github.com/luisresendez/anteroom/internal/queue"
)

func main() {
	err := run(os.Args[1:])
	switch {
	case err == nil, errors.Is(err, context.Canceled):
		// Interrupting anteroom is how you stop it, not a failure.
	case errors.Is(err, flag.ErrHelp):
		os.Exit(2)
	default:
		fmt.Fprintln(os.Stderr, "anteroom: "+err.Error())
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) > 0 && args[0] == "init" {
		return writeExampleConfig(args[1:])
	}

	fs := flag.NewFlagSet("anteroom", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprint(fs.Output(), usage)
		fs.PrintDefaults()
	}
	var (
		configPath = fs.String("config", "", "path to anteroom.yaml (omit to configure from flags)")
		origin     = fs.String("origin", "", "origin to protect, e.g. http://localhost:3000 (single-room mode)")
		rate       = fs.Float64("rate", config.DefaultRate, "visitors admitted per second (single-room mode)")
		maxActive  = fs.Int("max-active", config.DefaultMaxActive, "visitors allowed on the site at once (single-room mode)")
		listen     = fs.String("listen", "", "address to listen on (overrides the config file)")
		reseed     = fs.Bool("reseed", false, "overwrite live room settings with the ones in the config file")
		verbose    = fs.Bool("v", false, "log every admission pass")
	)
	if err := fs.Parse(args); err != nil {
		return err
	}

	level := slog.LevelInfo
	if *verbose {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	cfg, err := loadConfig(*configPath, *origin, *rate, *maxActive, log)
	if err != nil {
		return err
	}
	if *listen != "" {
		cfg.Listen = *listen
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// go-redis logs its own connection failures straight to stderr, which
	// buries anteroom's startup messages behind pool retry spam. Its errors
	// reach us as return values anyway, so we report them ourselves.
	redis.SetLogger(quietLogger{})

	rdb := redis.NewClient(&redis.Options{Addr: cfg.Redis.Addr})
	defer rdb.Close()
	if err := waitForRedis(ctx, rdb, log); err != nil {
		return err
	}
	store := queue.NewRedisStore(rdb)

	emitter := newEmitter(cfg, log)
	defer emitter.Close()

	if err := seedRooms(ctx, store, cfg, *reseed, log); err != nil {
		return err
	}

	srv, err := httpserver.New(cfg, store, emitter, log)
	if err != nil {
		return err
	}
	srv.Start(ctx)

	rooms := make([]string, 0, len(cfg.Rooms))
	for name := range cfg.Rooms {
		rooms = append(rooms, name)
	}
	go admit.New(store, emitter, rooms, cfg.AdmitInterval.Std(), log).Run(ctx)

	return serve(ctx, cfg, srv.Handler(), log)
}

const usage = `anteroom — a self-hosted virtual waiting room.

  anteroom --origin http://localhost:3000 --rate 50
  anteroom --config anteroom.yaml
  anteroom init > anteroom.yaml

Flags:
`

func loadConfig(path, origin string, rate float64, maxActive int, log *slog.Logger) (config.Config, error) {
	if path != "" {
		if origin != "" {
			return config.Config{}, errors.New("use either --config or --origin, not both")
		}
		return config.Load(path)
	}
	if origin == "" {
		return config.Config{}, errors.New("nothing to protect: pass --origin or --config (see --help)")
	}

	// Single-room mode: everything not set in the environment gets a sensible
	// default so that one flag is genuinely enough to start.
	cfg := config.Default()
	room := config.DefaultRoom(origin)
	room.Rate = rate
	room.MaxActive = maxActive
	room.Title = "Just a moment"
	cfg.Rooms["main"] = room

	if err := applyGeneratedSecrets(&cfg, log); err != nil {
		return config.Config{}, err
	}
	if err := cfg.ApplyEnvAndDefaults(); err != nil {
		return config.Config{}, err
	}
	return cfg, cfg.Validate()
}

// applyGeneratedSecrets fills in the two required secrets when the operator
// has not supplied them, so single-room mode needs no setup. The admin token
// is printed because it is the only way in to the dashboard.
func applyGeneratedSecrets(cfg *config.Config, log *slog.Logger) error {
	if cfg.CookieSecret == "" && os.Getenv("ANTEROOM_COOKIE_SECRET") == "" {
		secret, err := randomHex(32)
		if err != nil {
			return err
		}
		cfg.CookieSecret = secret
		log.Warn("anteroom: generated a temporary cookie secret; set ANTEROOM_COOKIE_SECRET to keep visitors queued across restarts")
	}
	if cfg.AdminToken == "" && os.Getenv("ANTEROOM_ADMIN_TOKEN") == "" {
		tok, err := randomHex(16)
		if err != nil {
			return err
		}
		cfg.AdminToken = tok
		log.Info("anteroom: generated an admin token for this run", "admin_token", tok)
	}
	return nil
}

// quietLogger discards go-redis's internal chatter.
type quietLogger struct{}

func (quietLogger) Printf(context.Context, string, ...any) {}

func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func newEmitter(cfg config.Config, log *slog.Logger) events.Emitter {
	if len(cfg.Kafka.Brokers) == 0 {
		log.Info("anteroom: no kafka brokers configured, event stream disabled")
		return events.Nop{}
	}
	log.Info("anteroom: publishing events", "brokers", cfg.Kafka.Brokers, "topic", cfg.Kafka.Topic)
	return events.NewKafka(cfg.Kafka.Brokers, cfg.Kafka.Topic, log)
}

// waitForRedis retries briefly so anteroom and Redis can start together.
func waitForRedis(ctx context.Context, rdb *redis.Client, log *slog.Logger) error {
	deadline := time.Now().Add(30 * time.Second)
	for attempt := 1; ; attempt++ {
		err := rdb.Ping(ctx).Err()
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("redis is unreachable: %w", err)
		}
		log.Warn("anteroom: waiting for redis", "attempt", attempt, "err", err)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

// seedRooms writes each room's settings into Redis. Existing values win unless
// the operator asked to reseed, so a rate raised from the dashboard is not
// silently undone by a restart.
func seedRooms(ctx context.Context, store queue.Store, cfg config.Config, reseed bool, log *slog.Logger) error {
	for name, room := range cfg.Rooms {
		// A fresh salt is offered every start, but seeding keeps the first one
		// forever: changing it would redraw a room mid-draw.
		salt, err := randomHex(16)
		if err != nil {
			return err
		}
		want := queue.RoomConfig{
			Rate:         room.Rate,
			MaxActive:    room.MaxActive,
			SessionTTL:   room.SessionTTL.Std(),
			AbandonAfter: room.AbandonAfter.Std(),
			JoinLimit:    room.JoinLimit(),
			JoinWindow:   room.JoinLimitWindow.Std(),
			Lottery:      room.Lottery,
			DrawSalt:     salt,
			QueueOpensAt: room.Schedule.QueueOpensAt.Time,
			AdmitsAt:     room.Schedule.AdmitsAt.Time,
			ClosesAt:     room.Schedule.ClosesAt.Time,
		}
		if err := store.Seed(ctx, name, want, reseed); err != nil {
			return err
		}
		snap, err := store.Snapshot(ctx, name)
		if err != nil {
			return err
		}
		attrs := []any{
			"room", name,
			"host", hostLabel(room.MatchHost),
			"origin", room.Origin,
			"rate", snap.Rate,
			"max_active", snap.MaxActive,
			"waiting", snap.Waiting,
		}
		if room.Schedule.IsSet() {
			attrs = append(attrs, "phase", snap.Phase.String(), "lottery", snap.Lottery)
			if !room.Schedule.AdmitsAt.IsZero() {
				attrs = append(attrs, "admits_at", room.Schedule.AdmitsAt.Format(time.RFC3339))
			}
		}
		log.Info("anteroom: room ready", attrs...)
		if snap.Rate != room.Rate || snap.MaxActive != room.MaxActive {
			log.Warn("anteroom: live settings differ from the config file; run with --reseed to replace them",
				"room", name,
				"live_rate", snap.Rate, "file_rate", room.Rate,
				"live_max_active", snap.MaxActive, "file_max_active", room.MaxActive,
			)
		}
	}
	return nil
}

func hostLabel(host string) string {
	if host == "" {
		return "(any)"
	}
	return host
}

func serve(ctx context.Context, cfg config.Config, handler http.Handler, log *slog.Logger) error {
	srv := &http.Server{
		Addr:    cfg.Listen,
		Handler: handler,
		// No WriteTimeout: it would cut off the position stream mid-wait.
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	errs := make(chan error, 1)
	go func() {
		log.Info("anteroom: listening", "addr", cfg.Listen, "dashboard", "http://"+cfg.Listen+"/__anteroom/admin/")
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errs <- err
		}
		close(errs)
	}()

	select {
	case err := <-errs:
		return err
	case <-ctx.Done():
		log.Info("anteroom: shutting down")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return srv.Shutdown(shutdownCtx)
}

func writeExampleConfig(args []string) error {
	out := os.Stdout
	if len(args) > 0 {
		f, err := os.OpenFile(args[0], os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return err
		}
		defer f.Close()
		out = f
	}
	secret, err := randomHex(32)
	if err != nil {
		return err
	}
	adminToken, err := randomHex(16)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(out, config.Example, secret, adminToken)
	return err
}
