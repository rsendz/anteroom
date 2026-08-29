package httpserver

import (
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"slices"
	"time"

	"github.com/luisresendez/anteroom/internal/events"
	"github.com/luisresendez/anteroom/internal/queue"
)

func (s *Server) registerAdmin(mux *http.ServeMux) {
	api := Prefix + "admin/api/"
	guard := s.requireAdmin

	// Answerable without touching Redis, because the queue store being
	// unreachable is exactly when someone will be looking at this.
	mux.Handle("GET "+api+"status", guard(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, s.health.status())
	}))
	mux.Handle("GET "+api+"rooms", guard(s.handleListRooms))
	mux.Handle("GET "+api+"rooms/{room}/stats", guard(s.handleRoomStats))
	mux.Handle("PUT "+api+"rooms/{room}/config", guard(s.handleSetConfig))
	mux.Handle("POST "+api+"rooms/{room}/pause", guard(s.handlePause(true)))
	mux.Handle("POST "+api+"rooms/{room}/resume", guard(s.handlePause(false)))
	mux.Handle("POST "+api+"rooms/{room}/flush", guard(s.handleFlush))

	// The dashboard shell is public; it is an empty page until someone supplies
	// a token, and every byte of data behind it is guarded above.
	mux.HandleFunc("GET "+Prefix+"admin/{$}", s.handleDashboard)
	mux.HandleFunc("GET "+Prefix+"admin", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, Prefix+"admin/", http.StatusMovedPermanently)
	})
}

// requireAdmin checks the bearer token in constant time.
func (s *Server) requireAdmin(next http.HandlerFunc) http.Handler {
	want := []byte("Bearer " + s.cfg.AdminToken)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := []byte(r.Header.Get("Authorization"))
		if subtle.ConstantTimeCompare(got, want) != 1 {
			w.Header().Set("WWW-Authenticate", `Bearer realm="anteroom"`)
			writeJSON(w, http.StatusUnauthorized, errorBody{Error: "unauthorized"})
			return
		}
		next(w, r)
	})
}

type errorBody struct {
	Error string `json:"error"`
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(body)
}

// storeUnavailable reports a queue-store failure. Every admin route answers
// these the same way, so an operator reading a 502 always sees where it came
// from rather than a bare Redis error.
func storeUnavailable(w http.ResponseWriter, err error) {
	writeJSON(w, http.StatusBadGateway, errorBody{Error: "queue store unavailable: " + err.Error()})
}

// room resolves and validates the {room} path segment.
func (s *Server) room(w http.ResponseWriter, r *http.Request) (string, bool) {
	name := r.PathValue("room")
	if _, ok := s.cfg.Rooms[name]; !ok {
		writeJSON(w, http.StatusNotFound, errorBody{Error: "no such room"})
		return "", false
	}
	return name, true
}

func (s *Server) handleListRooms(w http.ResponseWriter, r *http.Request) {
	names := s.roomNames()

	type roomView struct {
		queue.Snapshot
		MatchHost string `json:"match_host"`
		Origin    string `json:"origin"`
	}
	out := make([]roomView, 0, len(names))
	for _, name := range names {
		snap, err := s.store.Snapshot(r.Context(), name)
		if err != nil {
			storeUnavailable(w, err)
			return
		}
		out = append(out, roomView{
			Snapshot:  snap,
			MatchHost: s.cfg.Rooms[name].MatchHost,
			Origin:    s.cfg.Rooms[name].Origin,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"rooms": out})
}

func (s *Server) handleRoomStats(w http.ResponseWriter, r *http.Request) {
	name, ok := s.room(w, r)
	if !ok {
		return
	}
	snap, err := s.store.Snapshot(r.Context(), name)
	if err != nil {
		storeUnavailable(w, err)
		return
	}
	writeJSON(w, http.StatusOK, snap)
}

// configPatch carries only the fields the operator wants to change.
type configPatch struct {
	Rate             *float64 `json:"rate"`
	MaxActive        *int     `json:"max_active"`
	SessionTTLSecs   *int64   `json:"session_ttl_secs"`
	AbandonAfterSecs *int64   `json:"abandon_after_secs"`
}

func (s *Server) handleSetConfig(w http.ResponseWriter, r *http.Request) {
	name, ok := s.room(w, r)
	if !ok {
		return
	}
	var patch configPatch
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&patch); err != nil {
		writeJSON(w, http.StatusBadRequest, errorBody{Error: "invalid JSON body"})
		return
	}

	// Start from what the room is doing now, so a patch of one field leaves
	// the rest exactly as it was.
	snap, err := s.store.Snapshot(r.Context(), name)
	if err != nil {
		storeUnavailable(w, err)
		return
	}
	cfg := queue.RoomConfig{
		Rate:         snap.Rate,
		MaxActive:    snap.MaxActive,
		SessionTTL:   time.Duration(snap.SessionTTLSecs) * time.Second,
		AbandonAfter: time.Duration(snap.AbandonAfterSecs) * time.Second,
	}
	if patch.Rate != nil {
		cfg.Rate = *patch.Rate
	}
	if patch.MaxActive != nil {
		cfg.MaxActive = *patch.MaxActive
	}
	if patch.SessionTTLSecs != nil {
		cfg.SessionTTL = time.Duration(*patch.SessionTTLSecs) * time.Second
	}
	if patch.AbandonAfterSecs != nil {
		cfg.AbandonAfter = time.Duration(*patch.AbandonAfterSecs) * time.Second
	}
	if msg := validateRoomConfig(cfg); msg != "" {
		writeJSON(w, http.StatusBadRequest, errorBody{Error: msg})
		return
	}

	if err := s.store.SetConfig(r.Context(), name, cfg); err != nil {
		storeUnavailable(w, err)
		return
	}
	s.emitter.Emit(events.New(events.TypeConfigChanged, name, "", map[string]any{
		"rate":                cfg.Rate,
		"max_active":          cfg.MaxActive,
		"session_ttl_secs":    int64(cfg.SessionTTL.Seconds()),
		"abandon_after_secs":  int64(cfg.AbandonAfter.Seconds()),
		"previous_rate":       snap.Rate,
		"previous_max_active": snap.MaxActive,
	}))
	s.respondWithSnapshot(w, r, name)
}

func validateRoomConfig(cfg queue.RoomConfig) string {
	switch {
	case cfg.Rate <= 0:
		return "rate must be greater than zero"
	case cfg.MaxActive <= 0:
		return "max_active must be greater than zero"
	case cfg.SessionTTL <= 0:
		return "session_ttl_secs must be greater than zero"
	case cfg.AbandonAfter <= 0:
		return "abandon_after_secs must be greater than zero"
	}
	return ""
}

func (s *Server) handlePause(paused bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		name, ok := s.room(w, r)
		if !ok {
			return
		}
		if err := s.store.SetPaused(r.Context(), name, paused); err != nil {
			storeUnavailable(w, err)
			return
		}
		s.emitter.Emit(events.New(events.TypeConfigChanged, name, "", map[string]any{"paused": paused}))
		s.respondWithSnapshot(w, r, name)
	}
}

func (s *Server) handleFlush(w http.ResponseWriter, r *http.Request) {
	name, ok := s.room(w, r)
	if !ok {
		return
	}
	removed, err := s.store.Flush(r.Context(), name)
	if err != nil {
		storeUnavailable(w, err)
		return
	}
	s.emitter.Emit(events.New(events.TypeConfigChanged, name, "", map[string]any{"flushed": removed}))
	s.log.Warn("anteroom: waiting queue flushed", "room", name, "removed", removed)
	writeJSON(w, http.StatusOK, map[string]any{"removed": removed})
}

// respondWithSnapshot returns the room's state after a change, so the
// dashboard never has to guess whether its edit took effect.
func (s *Server) respondWithSnapshot(w http.ResponseWriter, r *http.Request, name string) {
	snap, err := s.store.Snapshot(r.Context(), name)
	if err != nil {
		storeUnavailable(w, err)
		return
	}
	writeJSON(w, http.StatusOK, snap)
}

func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	data := map[string]any{
		"APIPath": Prefix + "admin/api/",
		"Scripts": s.assets.scriptsFor("admin"),
		"Styles":  s.assets.stylesFor("admin"),
		"Built":   s.assets.built("admin"),
	}
	if err := s.tmpl.ExecuteTemplate(w, "admin.html", data); err != nil {
		s.log.Error("anteroom: rendering the dashboard failed", "err", err)
	}
}

// roomNames lists configured rooms in a stable order.
func (s *Server) roomNames() []string {
	names := make([]string, 0, len(s.cfg.Rooms))
	for name := range s.cfg.Rooms {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}
