package httpserver

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/luisresendez/anteroom/internal/config"
	"github.com/luisresendez/anteroom/internal/events"
	"github.com/luisresendez/anteroom/internal/queue"
	"github.com/luisresendez/anteroom/internal/token"
)

// do issues an arbitrary method request against the harness.
func (h *harness) do(method, path, body string, opts ...func(*http.Request)) *http.Response {
	h.t.Helper()
	var rdr *strings.Reader = strings.NewReader(body)
	req, err := http.NewRequest(method, h.server.URL+path, rdr)
	if err != nil {
		h.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	for _, opt := range opts {
		opt(req)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	h.t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func decode[T any](t *testing.T, resp *http.Response) T {
	t.Helper()
	var out T
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	return out
}

const apiPath = Prefix + "admin/api/"

func tokenPayload(id, room string) token.Payload {
	return token.Payload{ID: id, Room: room, Status: token.StatusAdmitted}
}

func TestAdminRequiresTheRightToken(t *testing.T) {
	h := newHarness(t, nil)
	cases := []struct {
		name string
		opts []func(*http.Request)
	}{
		{"no header", nil},
		{"wrong token", []func(*http.Request){withAuth("nope")}},
		{"token without the scheme", []func(*http.Request){func(r *http.Request) {
			r.Header.Set("Authorization", adminToken)
		}}},
		{"prefix of the real token", []func(*http.Request){withAuth(adminToken[:5])}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := h.get(apiPath+"rooms", tc.opts...)
			if resp.StatusCode != http.StatusUnauthorized {
				t.Errorf("status = %d, want 401", resp.StatusCode)
			}
			if got := resp.Header.Get("WWW-Authenticate"); got == "" {
				t.Error("no WWW-Authenticate header on the 401")
			}
		})
	}

	if resp := h.get(apiPath+"rooms", withAuth(adminToken)); resp.StatusCode != http.StatusOK {
		t.Errorf("the correct token was rejected: status %d", resp.StatusCode)
	}
}

func TestListRoomsReportsEveryRoom(t *testing.T) {
	h := newHarness(t, map[string]config.Room{
		"shop":    {MatchHost: "shop.test"},
		"tickets": {MatchHost: "tickets.test"},
	})
	resp := h.get(apiPath+"rooms", withAuth(adminToken))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}

	out := decode[struct {
		Rooms []struct {
			queue.Snapshot
			MatchHost string `json:"match_host"`
			Origin    string `json:"origin"`
		} `json:"rooms"`
	}](t, resp)

	if len(out.Rooms) != 2 {
		t.Fatalf("listed %d rooms, want 2", len(out.Rooms))
	}
	// Sorted by name, so the order is stable for the dashboard.
	if out.Rooms[0].Room != "shop" || out.Rooms[1].Room != "tickets" {
		t.Errorf("rooms listed as %q and %q", out.Rooms[0].Room, out.Rooms[1].Room)
	}
	if out.Rooms[0].MatchHost != "shop.test" || out.Rooms[0].Origin == "" {
		t.Errorf("room view missing routing detail: %+v", out.Rooms[0])
	}
	if out.Rooms[0].Waiting != 12 || out.Rooms[0].Rate != 4 {
		t.Errorf("room view missing counters: %+v", out.Rooms[0])
	}
}

func TestRoomStatsAndUnknownRoom(t *testing.T) {
	h := newHarness(t, nil)

	snap := decode[queue.Snapshot](t, h.get(apiPath+"rooms/main/stats", withAuth(adminToken)))
	if snap.Room != "main" || snap.Waiting != 12 || snap.Active != 3 {
		t.Errorf("snapshot = %+v", snap)
	}

	resp := h.get(apiPath+"rooms/nosuch/stats", withAuth(adminToken))
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown room status = %d, want 404", resp.StatusCode)
	}
}

func TestSetConfigPatchesOnlyWhatIsGiven(t *testing.T) {
	h := newHarness(t, nil)

	resp := h.do(http.MethodPut, apiPath+"rooms/main/config", `{"rate": 25.5}`, withAuth(adminToken))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d: %s", resp.StatusCode, body(t, resp))
	}
	snap := decode[queue.Snapshot](t, resp)
	if snap.Rate != 25.5 {
		t.Errorf("rate = %v, want 25.5", snap.Rate)
	}
	// The untouched fields keep their previous values.
	if snap.MaxActive != 10 || snap.SessionTTLSecs != 300 || snap.AbandonAfterSecs != 60 {
		t.Errorf("a partial patch changed other settings: %+v", snap)
	}

	h.store.mu.Lock()
	got := h.store.config
	h.store.mu.Unlock()
	if got == nil || got.Rate != 25.5 || got.MaxActive != 10 {
		t.Errorf("store received %+v", got)
	}

	var changed *events.Event
	for _, e := range h.emitter.events() {
		if e.Type == events.TypeConfigChanged {
			changed = &e
		}
	}
	if changed == nil {
		t.Fatal("no config_changed event was emitted")
	}
	if changed.Meta["rate"] != 25.5 || changed.Meta["previous_rate"] != float64(4) {
		t.Errorf("event meta = %v", changed.Meta)
	}
}

func TestSetConfigRejectsNonsense(t *testing.T) {
	h := newHarness(t, nil)
	cases := []struct{ name, body, wantErr string }{
		{"zero rate", `{"rate": 0}`, "rate must be"},
		{"negative rate", `{"rate": -3}`, "rate must be"},
		{"zero cap", `{"max_active": 0}`, "max_active must be"},
		{"negative ttl", `{"session_ttl_secs": -1}`, "session_ttl_secs must be"},
		{"zero abandon", `{"abandon_after_secs": 0}`, "abandon_after_secs must be"},
		{"not json", `{`, "invalid JSON"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := h.do(http.MethodPut, apiPath+"rooms/main/config", tc.body, withAuth(adminToken))
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", resp.StatusCode)
			}
			if got := decode[errorBody](t, resp); !strings.Contains(got.Error, tc.wantErr) {
				t.Errorf("error = %q, want it to mention %q", got.Error, tc.wantErr)
			}
		})
	}
}

func TestPauseAndResume(t *testing.T) {
	h := newHarness(t, nil)

	snap := decode[queue.Snapshot](t, h.do(http.MethodPost, apiPath+"rooms/main/pause", "", withAuth(adminToken)))
	if !snap.Paused {
		t.Error("the room did not report itself paused")
	}

	snap = decode[queue.Snapshot](t, h.do(http.MethodPost, apiPath+"rooms/main/resume", "", withAuth(adminToken)))
	if snap.Paused {
		t.Error("the room did not report itself resumed")
	}

	var paused []any
	for _, e := range h.emitter.events() {
		if v, ok := e.Meta["paused"]; ok {
			paused = append(paused, v)
		}
	}
	if len(paused) != 2 || paused[0] != true || paused[1] != false {
		t.Errorf("pause events = %v, want [true false]", paused)
	}
}

func TestFlushEmptiesTheQueue(t *testing.T) {
	h := newHarness(t, nil)
	resp := h.do(http.MethodPost, apiPath+"rooms/main/flush", "", withAuth(adminToken))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	out := decode[struct {
		Removed int64 `json:"removed"`
	}](t, resp)
	if out.Removed != 9 {
		t.Errorf("removed = %d, want 9", out.Removed)
	}
	h.store.mu.Lock()
	defer h.store.mu.Unlock()
	if h.store.flushed != 1 {
		t.Errorf("store flushed %d times, want 1", h.store.flushed)
	}
}

func TestUnmatchedReservedPathsAreNeverSiteTraffic(t *testing.T) {
	h := newHarness(t, nil)
	// An admitted visitor must not be able to reach the origin through
	// anteroom's own namespace, and a mistyped admin call must not quietly
	// come back as a waiting page.
	h.store.setResolution("v1", queue.Resolution{Admitted: true})
	admitted := withCookie(h.signer.Sign(tokenPayload("v1", "main")))

	cases := []struct{ name, method, path string }{
		{"wrong method on an admin endpoint", http.MethodPost, apiPath + "rooms/main/stats"},
		{"unknown reserved path", http.MethodGet, Prefix + "does-not-exist"},
		{"unknown admin path", http.MethodGet, Prefix + "admin/api/nope"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := h.do(tc.method, tc.path, "", withAuth(adminToken), admitted)
			if resp.StatusCode != http.StatusNotFound {
				t.Errorf("status = %d, want 404", resp.StatusCode)
			}
			if resp.Header.Get("X-From-Origin") != "" {
				t.Error("a reserved path was proxied to the origin")
			}
			if resp.Header.Get("X-Anteroom-Status") == "waiting" {
				t.Error("a reserved path was answered with the waiting page")
			}
		})
	}
}

func TestDashboardShellIsServedWithoutAToken(t *testing.T) {
	h := newHarness(t, nil)
	resp := h.get(Prefix + "admin/")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	page := body(t, resp)
	if !strings.Contains(page, `id="root"`) {
		t.Errorf("dashboard shell looks wrong:\n%s", page)
	}
	if strings.Contains(page, adminToken) {
		t.Error("the admin token leaked into the dashboard HTML")
	}
}

func TestDashboardPathRedirects(t *testing.T) {
	h := newHarness(t, nil)
	resp := h.get(strings.TrimSuffix(Prefix+"admin", "/"))
	if resp.StatusCode != http.StatusMovedPermanently {
		t.Errorf("status = %d, want 301", resp.StatusCode)
	}
	if got := resp.Header.Get("Location"); got != Prefix+"admin/" {
		t.Errorf("Location = %q", got)
	}
}
