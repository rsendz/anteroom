package httpserver

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/rsendz/anteroom/internal/config"
)

// metricsBody scrapes the endpoint after forcing a statistics refresh, so the
// numbers under test are the store's rather than whatever the background
// refresher happened to have collected by now.
func metricsBody(t *testing.T, h *harness) string {
	t.Helper()
	h.srv.stats.refresh(context.Background())
	resp := h.get(apiPath+"metrics", withAuth(adminToken))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got := resp.Header.Get("Content-Type"); !strings.HasPrefix(got, "text/plain") {
		t.Errorf("Content-Type = %q, want the Prometheus text format", got)
	}
	return body(t, resp)
}

func TestMetricsNeedTheAdminToken(t *testing.T) {
	h := newHarness(t, nil)
	resp := h.get(apiPath + "metrics")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
	if strings.Contains(body(t, resp), "anteroom_waiting") {
		t.Error("metrics were served without a token")
	}
}

func TestMetricsReportEveryRoom(t *testing.T) {
	h := newHarness(t, map[string]config.Room{
		"shop":    {MatchHost: "shop.test"},
		"tickets": {MatchHost: "tickets.test"},
	})
	got := metricsBody(t, h)

	for _, want := range []string{
		`anteroom_waiting{room="shop"} 12`,
		`anteroom_waiting{room="tickets"} 12`,
		`anteroom_active{room="shop"} 3`,
		`anteroom_rate{room="shop"} 4`,
		`anteroom_max_active{room="shop"} 10`,
		`anteroom_paused{room="shop"} 0`,
		"# TYPE anteroom_waiting gauge",
		"# TYPE anteroom_admitted_total counter",
		"# HELP anteroom_waiting ",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("metrics missing %q:\n%s", want, got)
		}
	}

	// Every series a room reports must be declared exactly once, or a scrape
	// is rejected as a duplicate metric family.
	for _, m := range roomMetrics {
		if n := strings.Count(got, "# TYPE "+m.name+" "); n != 1 {
			t.Errorf("%s declared %d times, want 1", m.name, n)
		}
	}
}

func TestMetricsReportQueueHealth(t *testing.T) {
	h := newHarness(t, nil)
	if got := metricsBody(t, h); !strings.Contains(got, "anteroom_queue_healthy 1") ||
		!strings.Contains(got, "anteroom_failing_open 0") {
		t.Errorf("healthy anteroom reported wrongly:\n%s", got)
	}

	h.srv.health.observe(true)
	got := metricsBody(t, h)
	if !strings.Contains(got, "anteroom_queue_healthy 0") {
		t.Errorf("an unreachable queue store was reported healthy:\n%s", got)
	}
}

// A scrape arriving mid-outage must still answer: a graph going flat is far
// easier to read during an incident than one that stops.
func TestMetricsSurviveTheStoreBeingDown(t *testing.T) {
	h := newHarness(t, nil)
	h.srv.stats.refresh(context.Background())
	h.store.setSnapshotErr(errors.New("redis is gone"))

	resp := h.get(apiPath+"metrics", withAuth(adminToken))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 from cached numbers", resp.StatusCode)
	}
	if got := body(t, resp); !strings.Contains(got, `anteroom_waiting{room="main"} 12`) {
		t.Errorf("cached numbers were not served during an outage:\n%s", got)
	}
}

func TestMetricLabelsAreEscaped(t *testing.T) {
	if got := escapeLabel(`a"b\c`); got != `a\"b\\c` {
		t.Errorf("escapeLabel = %q", got)
	}
}
