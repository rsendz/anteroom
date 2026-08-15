package httpserver

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/luisresendez/anteroom/internal/config"
	"github.com/luisresendez/anteroom/internal/events"
	"github.com/luisresendez/anteroom/internal/queue"
	"github.com/luisresendez/anteroom/internal/token"
)

const (
	testSecret = "0123456789abcdef0123456789abcdef"
	adminToken = "admin-token"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// fakeStore is a scriptable Store. Tests set what Resolve should report and
// inspect what the handler did with it.
type fakeStore struct {
	mu sync.Mutex

	resolution  map[string]queue.Resolution
	resolveErr  error
	snapshot    queue.Snapshot
	snapshotErr error

	resolveCalls []string
	buckets      []string
	refuseBucket string
	config       *queue.RoomConfig
	paused       *bool
	flushed      int
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		resolution: map[string]queue.Resolution{},
		snapshot: queue.Snapshot{
			Room: "main", Waiting: 12, Active: 3, Rate: 4, MaxActive: 10,
			SessionTTLSecs: 300, AbandonAfterSecs: 60,
		},
	}
}

func (f *fakeStore) Resolve(_ context.Context, room, id, bucket string) (queue.Resolution, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resolveCalls = append(f.resolveCalls, room+"/"+id)
	f.buckets = append(f.buckets, bucket)
	if f.resolveErr != nil {
		return queue.Resolution{}, f.resolveErr
	}
	if f.refuseBucket != "" && bucket == f.refuseBucket {
		return queue.Resolution{Refused: true}, nil
	}
	res, ok := f.resolution[id]
	if !ok {
		// Anyone the test did not script is a new visitor at the back.
		return queue.Resolution{Position: 7, Joined: true}, nil
	}
	return res, nil
}

func (f *fakeStore) lastBucket() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.buckets) == 0 {
		return ""
	}
	return f.buckets[len(f.buckets)-1]
}

func (f *fakeStore) Admit(context.Context, string) (queue.AdmitResult, error) {
	return queue.AdmitResult{}, nil
}

func (f *fakeStore) Snapshot(_ context.Context, room string) (queue.Snapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.snapshotErr != nil {
		return queue.Snapshot{}, f.snapshotErr
	}
	snap := f.snapshot
	snap.Room = room
	return snap, nil
}

func (f *fakeStore) SetConfig(_ context.Context, _ string, cfg queue.RoomConfig) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.config = &cfg
	f.snapshot.Rate = cfg.Rate
	f.snapshot.MaxActive = cfg.MaxActive
	f.snapshot.SessionTTLSecs = int64(cfg.SessionTTL.Seconds())
	f.snapshot.AbandonAfterSecs = int64(cfg.AbandonAfter.Seconds())
	return nil
}

func (f *fakeStore) SetPaused(_ context.Context, _ string, paused bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.paused = &paused
	f.snapshot.Paused = paused
	return nil
}

func (f *fakeStore) Flush(_ context.Context, _ string) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.flushed++
	return 9, nil
}

func (f *fakeStore) Seed(context.Context, string, queue.RoomConfig, bool) error { return nil }

func (f *fakeStore) setResolution(id string, res queue.Resolution) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resolution[id] = res
}

func (f *fakeStore) setResolveErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resolveErr = err
}

func (f *fakeStore) calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.resolveCalls...)
}

type recorder struct {
	mu  sync.Mutex
	got []events.Event
}

func (r *recorder) Emit(e events.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.got = append(r.got, e)
}
func (r *recorder) Close() error { return nil }
func (r *recorder) events() []events.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]events.Event(nil), r.got...)
}

// harness is a running anteroom in front of a stub origin.
type harness struct {
	t          *testing.T
	server     *httptest.Server
	origin     *httptest.Server
	store      *fakeStore
	emitter    *recorder
	signer     *token.Signer
	originHits *int64
}

func newHarness(t *testing.T, rooms map[string]config.Room) *harness {
	t.Helper()
	return newHarnessWith(t, rooms, nil)
}

// newHarnessWith allows the test to declare trusted proxies. Because the
// harness talks to a real listener, the peer address is always loopback;
// trusting it is how a test controls the apparent client address, which is
// also exactly how anteroom runs behind a load balancer.
func newHarnessWith(t *testing.T, rooms map[string]config.Room, trustedProxies []string) *harness {
	t.Helper()

	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-From-Origin", "yes")
		w.Write([]byte("origin:" + r.URL.Path))
	}))
	t.Cleanup(origin.Close)

	if rooms == nil {
		rooms = map[string]config.Room{"main": {Origin: origin.URL}}
	} else {
		for name, room := range rooms {
			if room.Origin == "" {
				room.Origin = origin.URL
				rooms[name] = room
			}
		}
	}

	cfg := config.Default()
	cfg.CookieSecret = testSecret
	cfg.AdminToken = adminToken
	cfg.Rooms = rooms
	cfg.TrustedProxies = trustedProxies
	if err := cfg.ApplyEnvAndDefaults(); err != nil {
		t.Fatal(err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}

	store := newFakeStore()
	rec := &recorder{}
	srv, err := New(cfg, store, rec, discardLogger())
	if err != nil {
		t.Fatal(err)
	}
	// A short stream interval keeps the SSE tests quick.
	srv.sseInterval = 20 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	srv.Start(ctx)

	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	return &harness{t: t, server: ts, origin: origin, store: store, emitter: rec, signer: token.New(testSecret)}
}

// get issues a request without following redirects, so cookies and statuses
// can be inspected exactly as the browser would see them.
func (h *harness) get(path string, opts ...func(*http.Request)) *http.Response {
	h.t.Helper()
	req, err := http.NewRequest(http.MethodGet, h.server.URL+path, nil)
	if err != nil {
		h.t.Fatal(err)
	}
	for _, opt := range opts {
		opt(req)
	}
	client := &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := client.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	h.t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func withHost(host string) func(*http.Request) {
	return func(r *http.Request) { r.Host = host }
}

func withCookie(value string) func(*http.Request) {
	return func(r *http.Request) {
		r.AddCookie(&http.Cookie{Name: token.CookieName, Value: value})
	}
}

func withAuth(tok string) func(*http.Request) {
	return func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+tok) }
}

func body(t *testing.T, resp *http.Response) string {
	t.Helper()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func visitorCookie(t *testing.T, resp *http.Response) *http.Cookie {
	t.Helper()
	for _, c := range resp.Cookies() {
		if c.Name == token.CookieName {
			return c
		}
	}
	return nil
}

func TestNewVisitorIsQueuedAndCookied(t *testing.T) {
	h := newHarness(t, nil)
	resp := h.get("/some/page")

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	if got := resp.Header.Get("X-Anteroom-Status"); got != "waiting" {
		t.Errorf("X-Anteroom-Status = %q, want waiting", got)
	}
	if got := resp.Header.Get("Cache-Control"); !strings.Contains(got, "no-store") {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}
	if resp.Header.Get("X-From-Origin") != "" {
		t.Error("a queued visitor reached the origin")
	}
	page := body(t, resp)
	if !strings.Contains(page, `data-position="7"`) {
		t.Errorf("waiting page does not show the position:\n%s", page)
	}

	c := visitorCookie(t, resp)
	if c == nil {
		t.Fatal("no visitor cookie was issued")
	}
	p, ok := h.signer.Verify(c.Value)
	if !ok {
		t.Fatal("the issued cookie does not verify")
	}
	if p.Room != "main" || p.Status != token.StatusWaiting {
		t.Errorf("cookie payload = %+v", p)
	}
	if !c.HttpOnly || c.SameSite != http.SameSiteLaxMode {
		t.Errorf("cookie attributes: HttpOnly=%v SameSite=%v", c.HttpOnly, c.SameSite)
	}

	if got := h.emitter.events(); len(got) != 1 || got[0].Type != events.TypeVisitorJoined {
		t.Errorf("emitted %+v, want one visitor_joined", got)
	}
}

func TestAdmittedVisitorReachesOrigin(t *testing.T) {
	h := newHarness(t, nil)
	h.store.setResolution("admitted-id", queue.Resolution{Admitted: true})
	tok := h.signer.Sign(token.Payload{ID: "admitted-id", Room: "main", Status: token.StatusAdmitted})

	resp := h.get("/dashboard", withCookie(tok))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if resp.Header.Get("X-From-Origin") != "yes" {
		t.Fatal("an admitted visitor did not reach the origin")
	}
	if got := body(t, resp); got != "origin:/dashboard" {
		t.Errorf("origin saw %q, want the original path", got)
	}
	// The cookie already says admitted, so there is nothing to re-set.
	if c := visitorCookie(t, resp); c != nil {
		t.Error("an unnecessary cookie was re-issued on the hot path")
	}
}

func TestJustAdmittedVisitorGetsUpgradedCookie(t *testing.T) {
	h := newHarness(t, nil)
	h.store.setResolution("v1", queue.Resolution{Admitted: true})
	waiting := h.signer.Sign(token.Payload{ID: "v1", Room: "main", Status: token.StatusWaiting})

	resp := h.get("/", withCookie(waiting))
	if resp.Header.Get("X-From-Origin") != "yes" {
		t.Fatal("the visitor was not proxied after being admitted")
	}
	c := visitorCookie(t, resp)
	if c == nil {
		t.Fatal("the cookie was not upgraded to admitted")
	}
	p, _ := h.signer.Verify(c.Value)
	if p.Status != token.StatusAdmitted {
		t.Errorf("cookie status = %q, want admitted", p.Status)
	}
}

func TestForgedAndForeignCookiesAreTreatedAsNewVisitors(t *testing.T) {
	h := newHarness(t, map[string]config.Room{
		"main":  {MatchHost: "main.test"},
		"other": {MatchHost: "other.test"},
	})
	h.store.setResolution("smuggled", queue.Resolution{Admitted: true})

	cases := []struct {
		name   string
		cookie string
		host   string
	}{
		{"garbage", "not-a-token", "main.test"},
		{"wrong secret", token.New("a-completely-different-secret").
			Sign(token.Payload{ID: "smuggled", Room: "main", Status: token.StatusAdmitted}), "main.test"},
		{"another room", token.New(testSecret).
			Sign(token.Payload{ID: "smuggled", Room: "other", Status: token.StatusAdmitted}), "main.test"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := h.get("/", withHost(tc.host), withCookie(tc.cookie))
			if resp.Header.Get("X-From-Origin") != "" {
				t.Error("a forged or foreign cookie got through to the origin")
			}
			if resp.Header.Get("X-Anteroom-Status") != "waiting" {
				t.Errorf("status header = %q, want waiting", resp.Header.Get("X-Anteroom-Status"))
			}
		})
	}
}

func TestQueueOutageHoldsVisitorsBack(t *testing.T) {
	h := newHarness(t, nil)
	h.store.setResolveErr(context.DeadlineExceeded)

	resp := h.get("/")
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", resp.StatusCode)
	}
	if resp.Header.Get("X-From-Origin") != "" {
		t.Error("visitors were waved through to the origin while the queue was down")
	}
	if got := resp.Header.Get("Retry-After"); got == "" {
		t.Error("no Retry-After on the degraded response")
	}
	if page := body(t, resp); !strings.Contains(page, `data-degraded="1"`) {
		t.Error("the waiting page does not tell the visitor something is wrong")
	}
}

func TestRoomsAreSelectedByHost(t *testing.T) {
	shop := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("shop-origin"))
	}))
	t.Cleanup(shop.Close)

	h := newHarness(t, map[string]config.Room{
		"shop":     {MatchHost: "shop.test", Origin: shop.URL},
		"fallback": {},
	})
	h.store.setResolution("v1", queue.Resolution{Admitted: true})
	shopCookie := h.signer.Sign(token.Payload{ID: "v1", Room: "shop", Status: token.StatusAdmitted})

	resp := h.get("/", withHost("shop.test"), withCookie(shopCookie))
	if got := body(t, resp); got != "shop-origin" {
		t.Errorf("shop host reached %q, want the shop origin", got)
	}

	// The same cookie is meaningless on the catch-all room's host.
	resp = h.get("/", withHost("anything-else.test"), withCookie(shopCookie))
	if resp.Header.Get("X-Anteroom-Status") != "waiting" {
		t.Error("a shop cookie was honoured by the fallback room")
	}
}

func TestUnknownHostWithoutCatchAll(t *testing.T) {
	h := newHarness(t, map[string]config.Room{"shop": {MatchHost: "shop.test"}})
	resp := h.get("/", withHost("nobody.test"))
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

func TestReservedPathsAreNeverProxied(t *testing.T) {
	h := newHarness(t, nil)
	// Even an admitted visitor must not be able to reach the origin through
	// anteroom's own namespace.
	h.store.setResolution("v1", queue.Resolution{Admitted: true})
	tok := h.signer.Sign(token.Payload{ID: "v1", Room: "main", Status: token.StatusAdmitted})

	resp := h.get(Prefix+"healthz", withCookie(tok))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("healthz status = %d", resp.StatusCode)
	}
	if got := body(t, resp); !strings.Contains(got, "ok") {
		t.Errorf("healthz said %q", got)
	}
	if resp.Header.Get("X-From-Origin") != "" {
		t.Error("a reserved path was proxied to the origin")
	}
}

func TestOriginFailureIsReportedPlainly(t *testing.T) {
	h := newHarness(t, nil)
	h.origin.Close() // the site behind the waiting room falls over
	h.store.setResolution("v1", queue.Resolution{Admitted: true})
	tok := h.signer.Sign(token.Payload{ID: "v1", Room: "main", Status: token.StatusAdmitted})

	resp := h.get("/", withCookie(tok))
	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", resp.StatusCode)
	}
	if got := body(t, resp); !strings.Contains(got, "not responding") {
		t.Errorf("body = %q", got)
	}
}

func TestRefusedVisitorGets429AndNoCookie(t *testing.T) {
	h := newHarnessWith(t, nil, []string{"127.0.0.0/8", "::1/128"})
	h.store.mu.Lock()
	h.store.refuseBucket = "203.0.113.5"
	h.store.mu.Unlock()

	resp := h.get("/", func(r *http.Request) { r.Header.Set("X-Forwarded-For", "203.0.113.5") })
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", resp.StatusCode)
	}
	if got := resp.Header.Get("Retry-After"); got == "" {
		t.Error("no Retry-After on the refusal")
	}
	if got := resp.Header.Get("X-Anteroom-Status"); got != "refused" {
		t.Errorf("X-Anteroom-Status = %q, want refused", got)
	}
	if resp.Header.Get("X-From-Origin") != "" {
		t.Error("a refused visitor reached the origin")
	}
	// No place was granted, so there is nothing to remember them by.
	if c := visitorCookie(t, resp); c != nil {
		t.Error("a cookie was issued to a refused visitor")
	}
	if page := body(t, resp); !strings.Contains(page, "Too many at once") {
		t.Errorf("refusal page looks wrong:\n%s", page)
	}
}

func TestJoinBucketComesFromTrustedForwardedHeader(t *testing.T) {
	h := newHarnessWith(t, nil, []string{"127.0.0.0/8", "::1/128"})
	h.get("/", func(r *http.Request) { r.Header.Set("X-Forwarded-For", "198.51.100.7") })
	if got := h.store.lastBucket(); got != "198.51.100.7" {
		t.Errorf("store received bucket %q, want the forwarded client address", got)
	}
}

func TestJoinBucketIgnoresSpoofedForwardedHeader(t *testing.T) {
	// No trusted proxies: a visitor must not be able to pick their own bucket
	// by setting the header, or the limit is trivially escaped.
	h := newHarness(t, nil)
	h.get("/", func(r *http.Request) { r.Header.Set("X-Forwarded-For", "203.0.113.99") })
	if got := h.store.lastBucket(); got == "203.0.113.99" {
		t.Error("a spoofed X-Forwarded-For chose the rate-limit bucket")
	}
}

func TestJoinBucketGroupsIPv6Clients(t *testing.T) {
	h := newHarnessWith(t, nil, []string{"127.0.0.0/8", "::1/128"})
	h.get("/", func(r *http.Request) { r.Header.Set("X-Forwarded-For", "2001:db8:1:2::9") })
	if got := h.store.lastBucket(); got != "2001:db8:1:2::/64" {
		t.Errorf("store received bucket %q, want the containing /64", got)
	}
}

func TestETAEstimate(t *testing.T) {
	cases := []struct {
		name     string
		position int64
		snap     queue.Snapshot
		want     int64
	}{
		{"ten at two per second", 10, queue.Snapshot{Rate: 2}, 5},
		{"rounds up", 10, queue.Snapshot{Rate: 3}, 4},
		{"paused has no estimate", 10, queue.Snapshot{Rate: 2, Paused: true}, 0},
		{"no rate has no estimate", 10, queue.Snapshot{Rate: 0}, 0},
		{"not queued", 0, queue.Snapshot{Rate: 2}, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := etaSeconds(tc.position, tc.snap); got != tc.want {
				t.Errorf("etaSeconds(%d, %+v) = %d, want %d", tc.position, tc.snap, got, tc.want)
			}
		})
	}
}

func TestSSEStreamsPositionThenAdmission(t *testing.T) {
	h := newHarness(t, nil)
	h.store.setResolution("v1", queue.Resolution{Position: 4})
	tok := h.signer.Sign(token.Payload{ID: "v1", Room: "main", Status: token.StatusWaiting})

	req, _ := http.NewRequest(http.MethodGet, h.server.URL+Prefix+"events", nil)
	req.AddCookie(&http.Cookie{Name: token.CookieName, Value: tok})
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if got := resp.Header.Get("Content-Type"); got != "text/event-stream" {
		t.Fatalf("Content-Type = %q", got)
	}

	frames := make(chan string, 8)
	go func() {
		buf := make([]byte, 512)
		var acc strings.Builder
		for {
			n, err := resp.Body.Read(buf)
			if n > 0 {
				acc.Write(buf[:n])
				for {
					s := acc.String()
					i := strings.Index(s, "\n\n")
					if i < 0 {
						break
					}
					frames <- s[:i]
					acc.Reset()
					acc.WriteString(s[i+2:])
				}
			}
			if err != nil {
				close(frames)
				return
			}
		}
	}()

	// The first position frame carries the visitor's place and the room total.
	deadline := time.After(5 * time.Second)
	var sawPosition bool
	for !sawPosition {
		select {
		case frame, ok := <-frames:
			if !ok {
				t.Fatal("the stream closed before sending a position")
			}
			if !strings.Contains(frame, "event: position") {
				continue
			}
			sawPosition = true
			var update positionUpdate
			data := frame[strings.Index(frame, "data: ")+len("data: "):]
			if err := json.Unmarshal([]byte(data), &update); err != nil {
				t.Fatalf("position frame is not valid JSON: %q", data)
			}
			if update.Position != 4 || update.Waiting != 12 {
				t.Errorf("update = %+v, want position 4 of 12 waiting", update)
			}
			if update.ETASeconds != 1 {
				t.Errorf("eta = %d, want 1 (4 people at 4/sec)", update.ETASeconds)
			}
		case <-deadline:
			t.Fatal("no position frame arrived")
		}
	}

	// Once admitted, the stream says so and closes so the page can reload.
	h.store.setResolution("v1", queue.Resolution{Admitted: true})
	for {
		select {
		case frame, ok := <-frames:
			if !ok {
				t.Fatal("the stream closed without announcing admission")
			}
			if strings.Contains(frame, "event: admitted") {
				return
			}
		case <-deadline:
			t.Fatal("admission was never announced")
		}
	}
}

func TestSSERequiresACookie(t *testing.T) {
	h := newHarness(t, nil)
	cases := []struct {
		name string
		opts []func(*http.Request)
	}{
		{"no cookie", nil},
		{"garbage cookie", []func(*http.Request){withCookie("nonsense")}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := h.get(Prefix+"events", tc.opts...)
			if resp.StatusCode != http.StatusPreconditionFailed {
				t.Errorf("status = %d, want 412", resp.StatusCode)
			}
		})
	}
}
