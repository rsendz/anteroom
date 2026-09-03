package httpserver

import (
	"bytes"
	"net/http"
	"strconv"
	"strings"

	"github.com/rsendz/anteroom/internal/queue"
)

// The Prometheus text exposition format. Writing it by hand rather than
// pulling in a client library keeps anteroom a single binary with no metrics
// dependency; the format is a few lines of text and it is versioned, so it
// does not drift underneath us.
const metricsContentType = "text/plain; version=0.0.4; charset=utf-8"

// roomMetric is one series reported per room.
type roomMetric struct {
	name string
	help string
	// kind is the Prometheus type: a gauge may go up or down, a counter only
	// ever climbs. Getting this wrong makes rate() over a counter meaningless,
	// so the totals are counters and the live numbers are gauges.
	kind  string
	value func(queue.Snapshot) float64
}

var roomMetrics = []roomMetric{
	{"anteroom_waiting", "Visitors currently in the waiting queue.", "gauge",
		func(s queue.Snapshot) float64 { return float64(s.Waiting) }},
	{"anteroom_active", "Visitors currently admitted to the site.", "gauge",
		func(s queue.Snapshot) float64 { return float64(s.Active) }},
	{"anteroom_max_active", "How many visitors may be on the site at once.", "gauge",
		func(s queue.Snapshot) float64 { return float64(s.MaxActive) }},
	{"anteroom_rate", "Configured admissions per second.", "gauge",
		func(s queue.Snapshot) float64 { return s.Rate }},
	{"anteroom_paused", "1 when an operator has paused admissions.", "gauge",
		func(s queue.Snapshot) float64 { return boolValue(s.Paused) }},
	{"anteroom_joined_total", "Visitors who have joined the queue.", "counter",
		func(s queue.Snapshot) float64 { return float64(s.TotalJoined) }},
	{"anteroom_admitted_total", "Visitors who have been let through to the site.", "counter",
		func(s queue.Snapshot) float64 { return float64(s.TotalAdmitted) }},
	{"anteroom_expired_total", "Admitted sessions reclaimed after going idle.", "counter",
		func(s queue.Snapshot) float64 { return float64(s.TotalExpired) }},
	{"anteroom_abandoned_total", "Waiting visitors dropped after they stopped waiting.", "counter",
		func(s queue.Snapshot) float64 { return float64(s.TotalAbandoned) }},
	{"anteroom_refused_total", "Joins turned away by the per-address limit.", "counter",
		func(s queue.Snapshot) float64 { return float64(s.TotalRefused) }},
}

// handleMetrics reports every room in the Prometheus text format.
//
// The numbers come from the statistics cache rather than Redis, for the same
// reason the waiting page does: a scrape costs no round trip, and a scrape
// arriving during an outage still answers with the last known numbers instead
// of failing at exactly the moment someone is looking at a graph.
func (s *Server) handleMetrics(w http.ResponseWriter, _ *http.Request) {
	names := s.roomNames()
	snaps := make(map[string]queue.Snapshot, len(names))
	for _, name := range names {
		snaps[name] = s.stats.get(name)
	}

	var buf bytes.Buffer
	for _, m := range roomMetrics {
		writeMetricHeader(&buf, m.name, m.help, m.kind)
		for _, name := range names {
			buf.WriteString(m.name)
			buf.WriteString(`{room="`)
			buf.WriteString(escapeLabel(name))
			buf.WriteString(`"} `)
			buf.WriteString(formatValue(m.value(snaps[name])))
			buf.WriteByte('\n')
		}
	}

	// Whether the queue store is answering is a property of this anteroom, not
	// of any one room, so it carries no label.
	health := s.health.status()
	writeMetricHeader(&buf, "anteroom_queue_healthy", "1 when the queue store is answering.", "gauge")
	buf.WriteString("anteroom_queue_healthy " + formatValue(boolValue(health.QueueHealthy)) + "\n")
	writeMetricHeader(&buf, "anteroom_failing_open", "1 while visitors are being let through unchecked.", "gauge")
	buf.WriteString("anteroom_failing_open " + formatValue(boolValue(health.FailingOpen)) + "\n")

	w.Header().Set("Content-Type", metricsContentType)
	w.Header().Set("Cache-Control", "no-store")
	w.Write(buf.Bytes())
}

func writeMetricHeader(buf *bytes.Buffer, name, help, kind string) {
	buf.WriteString("# HELP " + name + " " + help + "\n")
	buf.WriteString("# TYPE " + name + " " + kind + "\n")
}

// formatValue renders a sample. 'g' with the shortest round-tripping precision
// keeps whole numbers looking whole, which matters when an operator reads the
// endpoint by hand.
func formatValue(v float64) string {
	return strconv.FormatFloat(v, 'g', -1, 64)
}

func boolValue(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

// escapeLabel quotes a label value as the exposition format requires. Room
// names come from the operator's own config, but an unescaped quote would
// produce a scrape that silently fails to parse rather than an obvious error.
var labelEscaper = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)

func escapeLabel(v string) string { return labelEscaper.Replace(v) }
