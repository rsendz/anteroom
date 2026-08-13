// Package httpserver is anteroom's front door. Every request either reaches
// the origin through the reverse proxy or is answered with the waiting page;
// anteroom's own endpoints live under a reserved path prefix so that the site
// behind it needs no changes at all.
package httpserver

import (
	"context"
	"embed"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sync"
	"sync/atomic"
	"time"

	"github.com/luisresendez/anteroom/internal/config"
	"github.com/luisresendez/anteroom/internal/events"
	"github.com/luisresendez/anteroom/internal/queue"
	"github.com/luisresendez/anteroom/internal/token"
)

// Prefix is the URL space anteroom reserves for itself. Everything else
// belongs to the site being protected.
const Prefix = "/__anteroom/"

//go:embed all:web
var webFS embed.FS

// Server routes visitors to the origin or to the waiting page.
type Server struct {
	cfg     config.Config
	store   queue.Store
	signer  *token.Signer
	emitter events.Emitter
	log     *slog.Logger

	proxies    map[string]*httputil.ReverseProxy
	tmpl       *template.Template
	stats      *statsCache
	assets     assets
	ipResolver *clientIPResolver
	// lastProxyWarning rate-limits the untrusted-proxy warning (unix seconds).
	lastProxyWarning atomic.Int64

	// sseInterval is how often a waiting page is told its position.
	sseInterval time.Duration
}

// New builds a server for the given configuration.
func New(cfg config.Config, store queue.Store, emitter events.Emitter, log *slog.Logger) (*Server, error) {
	tmpl, err := template.ParseFS(webFS, "web/*.html")
	if err != nil {
		return nil, err
	}
	ipResolver, err := newClientIPResolver(cfg.TrustedProxies)
	if err != nil {
		return nil, fmt.Errorf("trusted_proxies: %w", err)
	}
	s := &Server{
		cfg:         cfg,
		store:       store,
		signer:      token.New(cfg.CookieSecret),
		emitter:     emitter,
		log:         log,
		proxies:     make(map[string]*httputil.ReverseProxy, len(cfg.Rooms)),
		tmpl:        tmpl,
		stats:       newStatsCache(store, cfg, log),
		assets:      loadAssets(webFS, log),
		ipResolver:  ipResolver,
		sseInterval: 2 * time.Second,
	}
	for name, room := range cfg.Rooms {
		proxy, err := s.newProxy(name, room)
		if err != nil {
			return nil, err
		}
		s.proxies[name] = proxy
	}
	return s, nil
}

// Handler returns the root handler: anteroom's own endpoints, and everything
// else funnelled through the waiting room.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET "+Prefix+"healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("GET "+Prefix+"events", s.handleEvents)

	static, err := fs.Sub(webFS, "web/static")
	if err == nil {
		mux.Handle("GET "+Prefix+"static/", http.StripPrefix(Prefix+"static/", cacheStatic(http.FileServerFS(static))))
	}

	s.registerAdmin(mux)

	// Everything else under the reserved prefix is anteroom's, even if no
	// handler claimed it. Without this, an unmatched /__anteroom/ path would
	// fall through to the visitor handler and be proxied to the origin,
	// quietly handing the reserved namespace to the site behind it.
	mux.HandleFunc(Prefix, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not an anteroom endpoint", http.StatusNotFound)
	})

	// Anything not claimed above is the protected site.
	mux.HandleFunc("/", s.handleVisitor)
	return mux
}

// Start runs the statistics refresher until ctx is cancelled.
func (s *Server) Start(ctx context.Context) { go s.stats.run(ctx) }

func (s *Server) newProxy(name string, room config.Room) (*httputil.ReverseProxy, error) {
	target, err := url.Parse(room.Origin)
	if err != nil {
		return nil, err
	}
	preserveHost := room.PreserveHost
	return &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			if preserveHost {
				pr.Out.Host = pr.In.Host
			}
			pr.SetXForwarded()
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			// The origin failing is not a queueing problem, so say so plainly
			// rather than sending the visitor back to the waiting page.
			s.log.Error("anteroom: origin unreachable", "room", name, "path", r.URL.Path, "err", err)
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.WriteHeader(http.StatusBadGateway)
			w.Write([]byte("The site behind this waiting room is not responding.\n"))
		},
	}, nil
}

func cacheStatic(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Vite fingerprints its filenames, so these are safe to cache hard.
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		h.ServeHTTP(w, r)
	})
}

// statsCache keeps a recent Snapshot per room so that rendering a waiting page
// or pushing an SSE update costs no extra Redis round trip. Under a spike the
// difference is one Redis call per second instead of one per waiting visitor.
type statsCache struct {
	store queue.Store
	log   *slog.Logger
	rooms []string

	mu   sync.RWMutex
	snap map[string]queue.Snapshot
}

func newStatsCache(store queue.Store, cfg config.Config, log *slog.Logger) *statsCache {
	c := &statsCache{store: store, log: log, snap: map[string]queue.Snapshot{}}
	for name, room := range cfg.Rooms {
		c.rooms = append(c.rooms, name)
		// Seed from the config file so the first page render has a sane rate
		// to estimate waits with, before the first refresh lands.
		c.snap[name] = queue.Snapshot{
			Room:      name,
			Rate:      room.Rate,
			MaxActive: room.MaxActive,
		}
	}
	return c
}

func (c *statsCache) run(ctx context.Context) {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	c.refresh(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			c.refresh(ctx)
		}
	}
}

func (c *statsCache) refresh(ctx context.Context) {
	for _, room := range c.rooms {
		snap, err := c.store.Snapshot(ctx, room)
		if err != nil {
			// Keep serving the previous numbers; they are only an estimate.
			if ctx.Err() == nil {
				c.log.Warn("anteroom: refreshing room statistics failed", "room", room, "err", err)
			}
			continue
		}
		c.mu.Lock()
		c.snap[room] = snap
		c.mu.Unlock()
	}
}

func (c *statsCache) get(room string) queue.Snapshot {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.snap[room]
}
