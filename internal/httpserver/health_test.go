package httpserver

import (
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/rsendz/anteroom/internal/config"
	"github.com/rsendz/anteroom/internal/events"
)

func TestFailOpenIsOffByDefault(t *testing.T) {
	h := newQueueHealth(false, time.Second)
	h.observe(true)
	h.now = func() time.Time { return time.Now().Add(time.Hour) }
	if h.shouldFailOpen() {
		t.Error("fail-open happened without being asked for")
	}
}

func TestFailOpenWaitsForTheGracePeriod(t *testing.T) {
	base := time.Now()
	h := newQueueHealth(true, 30*time.Second)
	h.now = func() time.Time { return base }

	if h.shouldFailOpen() {
		t.Error("a healthy queue reported as failing open")
	}

	h.observe(true)
	// A blip must not release the queue: that is the stampede anteroom exists
	// to prevent, arriving by a different route.
	for _, elapsed := range []time.Duration{0, time.Second, 29 * time.Second} {
		h.now = func() time.Time { return base.Add(elapsed) }
		if h.shouldFailOpen() {
			t.Errorf("failed open after only %v", elapsed)
		}
	}

	h.now = func() time.Time { return base.Add(31 * time.Second) }
	if !h.shouldFailOpen() {
		t.Error("did not fail open after the grace period elapsed")
	}
}

func TestRecoveryClosesAgain(t *testing.T) {
	base := time.Now()
	h := newQueueHealth(true, time.Second)
	h.now = func() time.Time { return base }
	h.observe(true)
	h.now = func() time.Time { return base.Add(time.Minute) }
	if !h.shouldFailOpen() {
		t.Fatal("expected to be failing open")
	}

	h.observe(false)
	if h.shouldFailOpen() {
		t.Error("still failing open after the queue recovered")
	}
	if got := h.unhealthyFor(); got != 0 {
		t.Errorf("unhealthyFor = %v after recovery, want 0", got)
	}
}

func TestOutageClockStartsAtTheFirstFailure(t *testing.T) {
	// Later failures must not push the deadline back, or a steady stream of
	// errors would postpone failing open indefinitely.
	base := time.Now()
	h := newQueueHealth(true, 10*time.Second)
	h.now = func() time.Time { return base }
	h.observe(true)

	for _, elapsed := range []time.Duration{2, 4, 6, 8} {
		h.now = func() time.Time { return base.Add(elapsed * time.Second) }
		h.observe(true)
	}
	h.now = func() time.Time { return base.Add(11 * time.Second) }
	if !h.shouldFailOpen() {
		t.Error("repeated failures pushed the grace period back")
	}
}

func TestStatusDescribesTheIncident(t *testing.T) {
	base := time.Now()
	h := newQueueHealth(true, 30*time.Second)
	h.now = func() time.Time { return base }

	if got := h.status(); !got.QueueHealthy || got.FailingOpen {
		t.Errorf("healthy status = %+v", got)
	}

	h.observe(true)
	h.now = func() time.Time { return base.Add(5 * time.Second) }
	got := h.status()
	if got.QueueHealthy || got.FailingOpen {
		t.Errorf("during grace = %+v, want unhealthy but not failing open", got)
	}
	if got.UnhealthySecs != 5 || got.Message == "" {
		t.Errorf("status = %+v", got)
	}

	h.now = func() time.Time { return base.Add(45 * time.Second) }
	got = h.status()
	if !got.FailingOpen {
		t.Errorf("after grace = %+v, want failing open", got)
	}
	if got.Message == "" {
		t.Error("no message explaining that the origin is unprotected")
	}
}

func TestFailOpenHoldsThenReleasesThenRecovers(t *testing.T) {
	h := newHarnessOpts(t, nil, nil, func(cfg *config.Config) {
		cfg.FailOpen = true
		cfg.FailOpenAfter = config.Duration(30 * time.Second)
	})
	base := time.Now()
	h.srv.health.now = func() time.Time { return base }
	h.store.setResolveErr(errors.New("redis is down"))

	// Inside the grace period visitors are held, so a blip releases nobody.
	resp := h.get("/")
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d during the grace period, want 503", resp.StatusCode)
	}
	if resp.Header.Get("X-From-Origin") != "" {
		t.Fatal("a visitor reached the origin during the grace period")
	}

	// Once the outage outlasts the grace period, the site is served rather
	// than nobody being served at all.
	h.srv.health.now = func() time.Time { return base.Add(31 * time.Second) }
	resp = h.get("/")
	if resp.Header.Get("X-From-Origin") != "yes" {
		t.Fatalf("status %d: visitors were still held after the grace period", resp.StatusCode)
	}

	// It must be loud, not silent.
	var announced bool
	for _, e := range h.emitter.events() {
		if e.Type == events.TypeFailingOpen {
			announced = true
		}
	}
	if !announced {
		t.Error("no event announced that the origin is unprotected")
	}

	// And it must close again the moment the queue answers.
	h.store.setResolveErr(nil)
	resp = h.get("/")
	if resp.Header.Get("X-Anteroom-Status") != "waiting" {
		t.Error("visitors were not queued again after the queue recovered")
	}
	if h.srv.health.shouldFailOpen() {
		t.Error("still failing open after recovery")
	}
}

func TestFailOpenDisabledKeepsHolding(t *testing.T) {
	h := newHarness(t, nil)
	base := time.Now()
	h.srv.health.now = func() time.Time { return base }
	h.store.setResolveErr(errors.New("redis is down"))
	h.get("/")

	h.srv.health.now = func() time.Time { return base.Add(time.Hour) }
	resp := h.get("/")
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503: fail-open must stay off unless asked for", resp.StatusCode)
	}
	if resp.Header.Get("X-From-Origin") != "" {
		t.Error("visitors reached the origin without fail-open being enabled")
	}
}

func TestStatusEndpointWorksWithoutTheQueue(t *testing.T) {
	// The dashboard has to be readable during exactly the incident it reports.
	h := newHarnessOpts(t, nil, nil, func(cfg *config.Config) { cfg.FailOpen = true })
	h.store.setSnapshotErr(errors.New("redis is down"))
	h.store.setResolveErr(errors.New("redis is down"))
	h.get("/")

	resp := h.get(Prefix+"admin/api/status", withAuth(adminToken))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status endpoint returned %d while the queue was down", resp.StatusCode)
	}
	got := decode[status](t, resp)
	if got.QueueHealthy {
		t.Error("reported the queue as healthy during an outage")
	}
	if !got.FailOpenEnabled || got.Message == "" {
		t.Errorf("status = %+v", got)
	}
}
