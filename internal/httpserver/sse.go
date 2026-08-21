package httpserver

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/luisresendez/anteroom/internal/queue"
	"github.com/luisresendez/anteroom/internal/token"
)

// positionUpdate is the payload the waiting page renders from.
type positionUpdate struct {
	Position   int64  `json:"position"`
	Waiting    int64  `json:"waiting"`
	ETASeconds int64  `json:"eta_secs"`
	Paused     bool   `json:"paused"`
	Phase      string `json:"phase"`
	// AdmitsAtMS and NowMS let the page count down against the server's clock
	// rather than the visitor's.
	AdmitsAtMS int64 `json:"admits_at_ms,omitempty"`
	NowMS      int64 `json:"now_ms"`
}

// handleEvents streams a waiting visitor their position until they are
// admitted. Polling on the server side rather than the client side means one
// Redis read per visitor every couple of seconds, no matter how impatient the
// visitor is; the connection itself doubles as the heartbeat that tells
// anteroom they are still there.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	roomName, ok := s.cfg.RoomForHost(r.Host)
	if !ok {
		http.Error(w, "no waiting room for this host", http.StatusNotFound)
		return
	}
	// Without a cookie there is nobody to report on; the page should reload
	// through the normal path, which will issue one.
	c, err := r.Cookie(token.CookieName)
	if err != nil {
		http.Error(w, "no visitor cookie", http.StatusPreconditionFailed)
		return
	}
	p, valid := s.signer.Verify(c.Value)
	if !valid || p.Room != roomName {
		http.Error(w, "stale visitor cookie", http.StatusPreconditionFailed)
		return
	}

	rc := http.NewResponseController(w)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Connection", "keep-alive")
	// Proxies that buffer responses would defeat the whole point of a stream.
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	// Tell the browser how long to wait before reconnecting if this drops.
	fmt.Fprintf(w, "retry: %d\n\n", s.sseInterval.Milliseconds())
	rc.Flush()

	ticker := time.NewTicker(s.sseInterval)
	defer ticker.Stop()

	joinBucket := s.joinBucket(r)

	for {
		res, err := s.store.Resolve(r.Context(), roomName, p.ID, joinBucket)
		if err != nil {
			if r.Context().Err() != nil {
				return
			}
			// Stay connected through a queue outage: the visitor keeps their
			// page, and updates resume when Redis does.
			s.log.Warn("anteroom: position update failed", "room", roomName, "err", err)
			if !writeEvent(w, rc, "stalled", struct{}{}) {
				return
			}
		} else if res.Refused {
			// Their address is over its join limit. Close the stream and let
			// the page reload into the normal path, which explains why.
			return
		} else if res.Admitted {
			// The page reloads on this, and the reload is what gets proxied.
			writeEvent(w, rc, "admitted", struct{}{})
			return
		} else {
			snap := s.stats.get(roomName)
			update := positionUpdate{
				Position:   res.Position,
				Waiting:    waitingTotal(snap.Waiting, res.Position),
				ETASeconds: etaSeconds(res.Position, snap),
				Paused:     snap.Paused,
				Phase:      res.Phase.String(),
				AdmitsAtMS: snap.AdmitsAtMS,
				NowMS:      time.Now().UnixMilli(),
			}
			// Nobody has a place until the doors open, so the page shows the
			// number of entrants and a countdown rather than a position.
			if res.Phase == queue.PhaseDraw || res.Phase == queue.PhaseBefore {
				update.Position = 0
				update.Waiting = snap.Waiting
				update.ETASeconds = 0
			}
			if !writeEvent(w, rc, "position", update) {
				return
			}
		}

		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
		}
	}
}

// writeEvent sends one SSE frame, reporting false once the visitor has gone.
func writeEvent(w http.ResponseWriter, rc *http.ResponseController, name string, payload any) bool {
	body, err := json.Marshal(payload)
	if err != nil {
		return false
	}
	if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", name, body); err != nil {
		return false
	}
	return rc.Flush() == nil
}
