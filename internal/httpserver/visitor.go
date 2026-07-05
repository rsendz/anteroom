package httpserver

import (
	"fmt"
	"math"
	"net/http"
	"strings"

	"github.com/luisresendez/anteroom/internal/config"
	"github.com/luisresendez/anteroom/internal/events"
	"github.com/luisresendez/anteroom/internal/queue"
	"github.com/luisresendez/anteroom/internal/token"
)

// visitor is who anteroom thinks is making a request, before the queue is
// consulted. known is false when there was no usable cookie, which is the
// signal to issue one.
type visitor struct {
	id     string
	status token.Status
	known  bool
}

// identify reads the visitor's cookie. A cookie that fails verification, or
// that belongs to a different room, is treated as no cookie at all: the
// visitor simply starts again rather than seeing an error.
func (s *Server) identify(r *http.Request, room string) visitor {
	if c, err := r.Cookie(token.CookieName); err == nil {
		if p, ok := s.signer.Verify(c.Value); ok && p.Room == room {
			return visitor{id: p.ID, status: p.Status, known: true}
		}
	}
	return visitor{id: token.NewID()}
}

func (s *Server) setCookie(w http.ResponseWriter, r *http.Request, room string, v visitor, status token.Status) {
	http.SetCookie(w, &http.Cookie{
		Name:     token.CookieName,
		Value:    s.signer.Sign(token.Payload{ID: v.id, Room: room, Status: status}),
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   s.cfg.SecureCookies || r.TLS != nil,
		MaxAge:   int(token.MaxAge.Seconds()),
	})
}

// handleVisitor is the front door: admitted visitors are proxied to the
// origin, everyone else gets the waiting page.
func (s *Server) handleVisitor(w http.ResponseWriter, r *http.Request) {
	roomName, ok := s.cfg.RoomForHost(r.Host)
	if !ok {
		s.renderUnknownHost(w, r)
		return
	}
	room := s.cfg.Rooms[roomName]
	v := s.identify(r, roomName)

	res, err := s.store.Resolve(r.Context(), roomName, v.id)
	if err != nil {
		if r.Context().Err() != nil {
			return // the visitor went away mid-request
		}
		// Without the queue there is no safe way to know who may pass, and
		// waving everyone through would hand the origin exactly the spike
		// anteroom exists to prevent. Hold visitors on the waiting page
		// instead, where they will be let in as soon as Redis is back.
		s.log.Error("anteroom: queue unavailable", "room", roomName, "err", err)
		s.renderWaiting(w, r, roomName, room, v, queue.Resolution{}, http.StatusServiceUnavailable)
		return
	}

	if res.Admitted {
		if v.status != token.StatusAdmitted {
			s.setCookie(w, r, roomName, v, token.StatusAdmitted)
		}
		s.proxies[roomName].ServeHTTP(w, r)
		return
	}

	if res.Joined {
		s.emitter.Emit(events.New(events.TypeVisitorJoined, roomName, v.id,
			map[string]any{"position": res.Position}))
	}
	if !v.known || v.status != token.StatusWaiting {
		s.setCookie(w, r, roomName, v, token.StatusWaiting)
	}
	s.renderWaiting(w, r, roomName, room, v, res, http.StatusOK)
}

// waitingPage is what the waiting page template renders from.
type waitingPage struct {
	Room    string
	Title   string
	Message string
	// Position is the visitor's place in line; 0 means it is not known yet,
	// which happens only while the queue store is unreachable.
	Position int64
	Waiting  int64
	// ETASeconds is 0 when no meaningful estimate can be made.
	ETASeconds int64
	Paused     bool
	Degraded   bool
	// ETAText is ETASeconds phrased the way a person would say it.
	ETAText string
	// RefreshSeconds drives the no-JavaScript fallback.
	RefreshSeconds int
	EventsPath     string
	Scripts        []string
	Styles         []string
	// PlainRefresh is set when the front-end bundle is not available, so the
	// page falls back to reloading itself instead of streaming updates.
	PlainRefresh bool
}

func (s *Server) renderWaiting(w http.ResponseWriter, r *http.Request, name string, room config.Room, v visitor, res queue.Resolution, status int) {
	snap := s.stats.get(name)
	eta := etaSeconds(res.Position, snap)
	page := waitingPage{
		Room:           name,
		Title:          room.Title,
		Message:        room.Message,
		Position:       res.Position,
		Waiting:        waitingTotal(snap.Waiting, res.Position),
		ETASeconds:     eta,
		ETAText:        etaText(eta),
		Paused:         snap.Paused,
		Degraded:       status != http.StatusOK,
		RefreshSeconds: 10,
		EventsPath:     Prefix + "events",
		Scripts:        s.assets.scriptsFor("queue"),
		Styles:         s.assets.stylesFor("queue"),
		PlainRefresh:   !s.assets.built("queue"),
	}
	if page.Title == "" {
		page.Title = name
	}

	// The waiting page is per-visitor and changes every few seconds, so it
	// must never be held by a browser or an intermediate cache.
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store, must-revalidate")
	w.Header().Set("X-Anteroom-Status", "waiting")
	if status != http.StatusOK {
		w.Header().Set("Retry-After", "10")
	}
	w.WriteHeader(status)

	if err := s.tmpl.ExecuteTemplate(w, "queue.html", page); err != nil {
		// The response is already partly written, so there is nothing useful
		// left to send the visitor.
		s.log.Error("anteroom: rendering the waiting page failed", "room", name, "err", err)
	}
}

func (s *Server) renderUnknownHost(w http.ResponseWriter, r *http.Request) {
	s.log.Warn("anteroom: no room matches this host", "host", r.Host, "path", r.URL.Path)
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusNotFound)
	w.Write([]byte("No waiting room is configured for " + sanitizeHost(r.Host) + ".\n"))
}

// sanitizeHost keeps a hostile Host header out of the response body verbatim.
func sanitizeHost(host string) string {
	if len(host) > 128 {
		host = host[:128]
	}
	return strings.Map(func(r rune) rune {
		if r < ' ' || r == 0x7f {
			return -1
		}
		return r
	}, host)
}

// etaSeconds estimates the wait from the room's admission rate. It is
// deliberately simple: the queue drains at `rate` visitors per second, so the
// visitor at position P waits about P/rate. It ignores stalls caused by a full
// site, which is why the page presents it as an estimate.
func etaSeconds(position int64, snap queue.Snapshot) int64 {
	if position <= 0 || snap.Paused || snap.Rate <= 0 {
		return 0
	}
	return int64(math.Ceil(float64(position) / snap.Rate))
}

// waitingTotal reconciles the room total with the visitor's own position. The
// total comes from a once-a-second cache, so a visitor who just joined can be
// standing at position 12 of a queue the cache still believes is empty. Their
// own position is a lower bound on how many people are in line.
func waitingTotal(cached, position int64) int64 {
	return max(cached, position)
}

// etaText rounds a wait to something a person would actually say out loud.
// The front-end phrases live updates the same way; this covers the first
// render and visitors without JavaScript.
func etaText(seconds int64) string {
	switch {
	case seconds <= 0:
		return ""
	case seconds < 60:
		rounded := max(5, ((seconds+4)/5)*5)
		return fmt.Sprintf("%d seconds", rounded)
	case seconds < 3600:
		minutes := (seconds + 59) / 60
		if minutes == 1 {
			return "a minute"
		}
		return fmt.Sprintf("%d minutes", minutes)
	default:
		hours := math.Round(float64(seconds)/360) / 10
		if hours == 1 {
			return "an hour"
		}
		return fmt.Sprintf("%g hours", hours)
	}
}
