// Command loadtest measures what one anteroom actually holds.
//
// It is deliberately not part of the module above it: a load test needs a
// running anteroom, a real Redis and several minutes, none of which belong in
// `make check`.
//
// The phases mirror what a spike does to a waiting room. Fill puts visitors in
// the queue as fast as the door will take them. Hold keeps position streams
// open, which is the resource that actually runs out, since every waiting
// visitor costs a socket, a goroutine and a Redis read every couple of
// seconds. Drain then measures whether admissions come out at the configured
// rate with the queue at full depth.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// visitorCookie mirrors token.CookieName. It is repeated rather than imported
// because that package is internal to the module above, which is the same
// reason this one stands alone.
const visitorCookie = "ar_visitor"

func main() {
	var (
		base     = flag.String("url", "http://127.0.0.1:8099", "anteroom base URL, or several comma-separated to drive replicas")
		token    = flag.String("token", "loadtest-admin-token", "admin token")
		room     = flag.String("room", "", "room to drive (default: the first one)")
		fill     = flag.Int("fill", 100000, "visitors to put in the queue")
		hold     = flag.Int("hold", 0, "position streams to open and hold")
		holdFor  = flag.Duration("hold-for", 30*time.Second, "how long to hold them")
		ramp     = flag.Duration("ramp", 15*time.Second, "spread the stream opens over this long")
		workers  = flag.Int("workers", 256, "concurrent requests during fill")
		pid      = flag.String("pid", "", "anteroom pids, comma-separated, to sample their RSS")
		redisAdr = flag.String("redis", "", "redis host:port, to sample its memory")
		rate     = flag.Float64("drain-rate", 0, "rate to set for the drain phase (0 skips it)")
		drainFor = flag.Duration("drain-for", 20*time.Second, "how long to measure the drain")
		host     = flag.String("host", "", "Host header, for a room matched by host")
		out      = flag.String("json", "", "write the results here as JSON")
	)
	flag.Parse()

	rig := &rig{token: *token, redis: *redisAdr, host: *host}
	for _, u := range strings.Split(*base, ",") {
		if u = strings.TrimRight(strings.TrimSpace(u), "/"); u != "" {
			rig.bases = append(rig.bases, u)
		}
	}
	for _, p := range strings.Split(*pid, ",") {
		if n, err := strconv.Atoi(strings.TrimSpace(p)); err == nil {
			rig.pids = append(rig.pids, n)
		}
	}
	if len(rig.bases) == 0 {
		log.Fatal("no -url given")
	}
	if *room == "" {
		name, err := rig.firstRoom()
		if err != nil {
			log.Fatalf("reading rooms: %v", err)
		}
		*room = name
	}
	rig.room = *room

	res := results{
		Room:     *room,
		Started:  time.Now().Format(time.RFC3339),
		Host:     hostFacts(),
		Replicas: len(rig.bases),
	}

	// Nobody is let through while the queue fills, so the depth being measured
	// is the depth actually reached rather than a race between the door and
	// the flood.
	if err := rig.pause(true); err != nil {
		log.Fatalf("pausing: %v", err)
	}
	defer rig.pause(false)

	fmt.Printf("filling the queue with %s visitors (%d workers)\n", commas(int64(*fill)), *workers)
	cookies, fillStats := rig.fillQueue(*fill, *workers, *hold)
	res.Fill = fillStats
	fmt.Printf("  %s joins in %.1fs = %s joins/sec, p50 %s, p99 %s\n",
		commas(fillStats.Joins), fillStats.Seconds, commas(int64(fillStats.PerSecond)),
		fillStats.P50, fillStats.P99)

	res.AtDepth = rig.sample()
	fmt.Printf("  queue depth %s, anteroom RSS %s, redis %s\n",
		commas(res.AtDepth.Waiting), mib(res.AtDepth.AnteroomRSSBytes), mib(res.AtDepth.RedisBytes))

	if *hold > 0 {
		fmt.Printf("holding %s position streams for %s\n", commas(int64(*hold)), *holdFor)
		res.Hold = rig.holdStreams(cookies, *holdFor, *ramp)
		fmt.Printf("  %s streams open, %s failed; page load under load p50 %s, p99 %s\n",
			commas(res.Hold.Open), commas(res.Hold.Failed), res.Hold.PageP50, res.Hold.PageP99)
		fmt.Printf("  anteroom RSS %s, redis %s\n",
			mib(res.Hold.AnteroomRSSBytes), mib(res.Hold.RedisBytes))
	}

	if *rate > 0 {
		fmt.Printf("draining at a configured %g/sec for %s\n", *rate, *drainFor)
		d, err := rig.measureDrain(*rate, *drainFor)
		if err != nil {
			log.Fatalf("drain: %v", err)
		}
		res.Drain = d
		fmt.Printf("  admitted %s in %.1fs = %.1f/sec observed (configured %g)\n",
			commas(d.Admitted), d.Seconds, d.PerSecond, d.Configured)
		fmt.Printf("  queue %s -> %s, a drop of %s\n",
			commas(d.WaitingBefore), commas(d.WaitingAfter), commas(d.QueueDrop))
	}

	res.Finished = time.Now().Format(time.RFC3339)
	if *out != "" {
		body, _ := json.MarshalIndent(res, "", "  ")
		if err := os.WriteFile(*out, append(body, '\n'), 0o644); err != nil {
			log.Fatalf("writing results: %v", err)
		}
		fmt.Printf("wrote %s\n", *out)
	}
}

type results struct {
	Room     string     `json:"room"`
	Started  string     `json:"started"`
	Finished string     `json:"finished"`
	Host     string     `json:"host"`
	Replicas int        `json:"replicas"`
	Fill     fillStats  `json:"fill"`
	AtDepth  sample     `json:"at_depth"`
	Hold     holdStats  `json:"hold,omitempty"`
	Drain    drainStats `json:"drain,omitempty"`
}

type fillStats struct {
	Joins     int64   `json:"joins"`
	Errors    int64   `json:"errors"`
	Seconds   float64 `json:"seconds"`
	PerSecond float64 `json:"joins_per_second"`
	P50       string  `json:"p50"`
	P99       string  `json:"p99"`
	Max       string  `json:"max"`
}

type sample struct {
	Waiting          int64 `json:"waiting"`
	Active           int64 `json:"active"`
	AnteroomRSSBytes int64 `json:"anteroom_rss_bytes"`
	RedisBytes       int64 `json:"redis_used_memory_bytes"`
}

type holdStats struct {
	Open             int64  `json:"streams_open"`
	Failed           int64  `json:"streams_failed"`
	Frames           int64  `json:"frames_received"`
	AnteroomRSSBytes int64  `json:"anteroom_rss_bytes"`
	RedisBytes       int64  `json:"redis_used_memory_bytes"`
	PageP50          string `json:"page_load_p50"`
	PageP99          string `json:"page_load_p99"`
}

type drainStats struct {
	Configured float64 `json:"configured_rate"`
	Admitted   int64   `json:"admitted"`
	Seconds    float64 `json:"seconds"`
	PerSecond  float64 `json:"observed_per_second"`
	// The queue either side of the window. A counter can be read wrong; the
	// queue shrinking by the same number is the check on it.
	WaitingBefore int64 `json:"waiting_before"`
	WaitingAfter  int64 `json:"waiting_after"`
	QueueDrop     int64 `json:"queue_drop"`
}

type rig struct {
	bases                    []string
	token, room, redis, host string
	pids                     []int

	// next round-robins requests across the replicas, so a run with several
	// anteroom processes spreads load the way a load balancer would.
	next atomic.Uint64
}

// base picks the next replica.
func (r *rig) base() string {
	if len(r.bases) == 1 {
		return r.bases[0]
	}
	return r.bases[r.next.Add(1)%uint64(len(r.bases))]
}

// admin always talks to the first replica. Every number it reports lives in
// Redis, so any replica would answer identically.
func (r *rig) admin() string { return r.bases[0] }

// fillQueue joins visitors as fast as the workers can. The first `keep`
// cookies are kept so the hold phase can reconnect as those same visitors,
// because a position stream only belongs to someone already in line.
func (r *rig) fillQueue(n, workers, keep int) ([]string, fillStats) {
	client := &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			MaxIdleConns:        workers,
			MaxIdleConnsPerHost: workers,
			MaxConnsPerHost:     workers,
			IdleConnTimeout:     60 * time.Second,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}

	var (
		joins, errs int64
		mu          sync.Mutex
		cookies     = make([]string, 0, keep)
		lat         = make([]time.Duration, 0, n)
	)

	jobs := make(chan int, workers)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range jobs {
				start := time.Now()
				req, _ := http.NewRequest(http.MethodGet, r.base()+"/", nil)
				r.setHost(req)
				resp, err := client.Do(req)
				if err != nil {
					atomic.AddInt64(&errs, 1)
					continue
				}
				io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
				took := time.Since(start)

				var cookie string
				for _, c := range resp.Cookies() {
					if c.Name == visitorCookie {
						cookie = c.Name + "=" + c.Value
					}
				}
				atomic.AddInt64(&joins, 1)
				mu.Lock()
				lat = append(lat, took)
				if cookie != "" && len(cookies) < keep {
					cookies = append(cookies, cookie)
				}
				mu.Unlock()
			}
		}()
	}

	start := time.Now()
	for i := 0; i < n; i++ {
		jobs <- i
	}
	close(jobs)
	wg.Wait()
	elapsed := time.Since(start)

	sort.Slice(lat, func(i, j int) bool { return lat[i] < lat[j] })
	perSecond := 0.0
	if secs := elapsed.Seconds(); secs > 0 && joins > 0 {
		perSecond = float64(joins) / secs
	}
	return cookies, fillStats{
		Joins:     joins,
		Errors:    errs,
		Seconds:   elapsed.Seconds(),
		PerSecond: perSecond,
		P50:       pct(lat, 0.50),
		P99:       pct(lat, 0.99),
		Max:       pct(lat, 1),
	}
}

// holdStreams opens one position stream per cookie and keeps them open, which
// is what a browser sitting on the waiting page does.
func (r *rig) holdStreams(cookies []string, d, ramp time.Duration) holdStats {
	// The ramp is added on so that every stream, including the last one opened,
	// is actually held for the requested duration.
	ctx, cancel := context.WithTimeout(context.Background(), d+ramp)
	defer cancel()

	client := &http.Client{
		Transport: &http.Transport{
			MaxIdleConns:        len(cookies) + 16,
			MaxIdleConnsPerHost: len(cookies) + 16,
			DisableCompression:  true,
		},
	}

	var open, failed, frames int64
	ready := make(chan struct{}, len(cookies))
	// Opened over a ramp rather than all at once. Ten thousand simultaneous
	// dials overrun the listen backlog and measure the accept queue instead of
	// what the server can hold, and no real crowd arrives in one instant.
	gap := time.Duration(0)
	if ramp > 0 && len(cookies) > 0 {
		gap = ramp / time.Duration(len(cookies))
	}
	var wg sync.WaitGroup
	for i, cookie := range cookies {
		wg.Add(1)
		go func(i int, cookie string) {
			defer wg.Done()
			if gap > 0 {
				time.Sleep(time.Duration(i) * gap)
			}
			req, _ := http.NewRequestWithContext(ctx, http.MethodGet, r.base()+"/__anteroom/events", nil)
			r.setHost(req)
			req.Header.Set("Cookie", cookie)
			req.Header.Set("Accept", "text/event-stream")
			resp, err := client.Do(req)
			if err != nil {
				atomic.AddInt64(&failed, 1)
				ready <- struct{}{}
				return
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				atomic.AddInt64(&failed, 1)
				ready <- struct{}{}
				return
			}
			atomic.AddInt64(&open, 1)
			ready <- struct{}{}

			// Read frames until the deadline, so the connection stays alive and
			// anteroom keeps billing us for it.
			// A small buffer on purpose: at tens of thousands of open streams
			// the load generator's own memory is a real constraint, and a
			// position frame is a couple of hundred bytes.
			sc := bufio.NewScanner(resp.Body)
			sc.Buffer(make([]byte, 512), 4096)
			for sc.Scan() {
				if strings.HasPrefix(sc.Text(), "event:") {
					atomic.AddInt64(&frames, 1)
				}
			}
		}(i, cookie)
	}

	// Wait for every stream to have reported before measuring anything, or the
	// sample lands while connections are still coming up.
	for i := 0; i < len(cookies); i++ {
		<-ready
	}
	time.Sleep(3 * time.Second)

	s := r.sample()
	pageLat := r.probePageLoad(24)

	wg.Wait()
	sort.Slice(pageLat, func(i, j int) bool { return pageLat[i] < pageLat[j] })
	return holdStats{
		Open:             atomic.LoadInt64(&open),
		Failed:           atomic.LoadInt64(&failed),
		Frames:           atomic.LoadInt64(&frames),
		AnteroomRSSBytes: s.AnteroomRSSBytes,
		RedisBytes:       s.RedisBytes,
		PageP50:          pct(pageLat, 0.50),
		PageP99:          pct(pageLat, 0.99),
	}
}

// probePageLoad times fresh waiting-page loads while everything else is going
// on, which is what a visitor arriving mid-spike experiences.
func (r *rig) probePageLoad(n int) []time.Duration {
	client := &http.Client{Timeout: 30 * time.Second}
	out := make([]time.Duration, 0, n)
	for i := 0; i < n; i++ {
		start := time.Now()
		req, _ := http.NewRequest(http.MethodGet, r.base()+"/", nil)
		r.setHost(req)
		resp, err := client.Do(req)
		if err != nil {
			continue
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		out = append(out, time.Since(start))
		time.Sleep(100 * time.Millisecond)
	}
	return out
}

// measureDrain reads the admitted counter off the metrics endpoint before and
// after, so the number is anteroom's own rather than the load test's guess.
func (r *rig) measureDrain(rate float64, d time.Duration) (drainStats, error) {
	if err := r.setRate(rate); err != nil {
		return drainStats{}, err
	}
	if err := r.pause(false); err != nil {
		return drainStats{}, err
	}
	before, err := r.admittedTotal()
	if err != nil {
		return drainStats{}, err
	}
	waitingBefore, _ := r.snapshot()
	start := time.Now()
	time.Sleep(d)
	after, err := r.admittedTotal()
	if err != nil {
		return drainStats{}, err
	}
	waitingAfter, _ := r.snapshot()
	elapsed := time.Since(start).Seconds()
	return drainStats{
		Configured:    rate,
		Admitted:      after - before,
		Seconds:       elapsed,
		PerSecond:     float64(after-before) / elapsed,
		WaitingBefore: waitingBefore.Waiting,
		WaitingAfter:  waitingAfter.Waiting,
		QueueDrop:     waitingBefore.Waiting - waitingAfter.Waiting,
	}, nil
}

func (r *rig) sample() sample {
	s := sample{}
	if snap, err := r.snapshot(); err == nil {
		s.Waiting, s.Active = snap.Waiting, snap.Active
	}
	s.AnteroomRSSBytes = r.rss()
	s.RedisBytes = r.redisMemory()
	return s
}

// rss reads the resident size of the anteroom process, which is the number an
// operator sizing a box actually cares about.
func (r *rig) rss() int64 {
	var total int64
	for _, pid := range r.pids {
		// Retried: forking ps out of a process holding tens of thousands of
		// connections can fail for want of resources, and losing the memory
		// number is losing the point of the run.
		var out []byte
		var err error
		for attempt := 0; attempt < 3; attempt++ {
			if out, err = exec.Command("ps", "-o", "rss=", "-p", strconv.Itoa(pid)).Output(); err == nil {
				break
			}
			time.Sleep(250 * time.Millisecond)
		}
		if err != nil {
			continue
		}
		kb, err := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
		if err != nil {
			continue
		}
		total += kb * 1024
	}
	return total
}

// redisMemory asks Redis how much it is using, over an inline command so the
// load test needs no Redis client library.
func (r *rig) redisMemory() int64 {
	if r.redis == "" {
		return 0
	}
	conn, err := net.DialTimeout("tcp", r.redis, 3*time.Second)
	if err != nil {
		return 0
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := conn.Write([]byte("INFO memory\r\n")); err != nil {
		return 0
	}
	sc := bufio.NewScanner(conn)
	sc.Buffer(make([]byte, 64*1024), 64*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if v, ok := strings.CutPrefix(line, "used_memory:"); ok {
			n, _ := strconv.ParseInt(v, 10, 64)
			return n
		}
	}
	return 0
}

type snapshot struct {
	Room          string `json:"room"`
	Waiting       int64  `json:"waiting"`
	Active        int64  `json:"active"`
	TotalAdmitted int64  `json:"total_admitted"`
}

func (r *rig) snapshot() (snapshot, error) {
	var s snapshot
	err := r.adminJSON(http.MethodGet, "/rooms/"+r.room+"/stats", nil, &s)
	return s, err
}

func (r *rig) firstRoom() (string, error) {
	var body struct {
		Rooms []snapshot `json:"rooms"`
	}
	if err := r.adminJSON(http.MethodGet, "/rooms", nil, &body); err != nil {
		return "", err
	}
	if len(body.Rooms) == 0 {
		return "", fmt.Errorf("no rooms configured")
	}
	return body.Rooms[0].Room, nil
}

func (r *rig) pause(paused bool) error {
	action := "/rooms/" + r.room + "/resume"
	if paused {
		action = "/rooms/" + r.room + "/pause"
	}
	return r.adminJSON(http.MethodPost, action, nil, nil)
}

func (r *rig) setRate(rate float64) error {
	body := strings.NewReader(fmt.Sprintf(`{"rate": %g}`, rate))
	return r.adminJSON(http.MethodPut, "/rooms/"+r.room+"/config", body, nil)
}

// admittedTotal reads the admission counter live from Redis.
//
// Deliberately not the metrics endpoint: that one answers from the
// one-second statistics cache, which is right for a scraper and wrong here.
// At twenty thousand admissions a second, a cache that is a second stale puts
// twenty thousand admissions of error into a delta, which is exactly the gap
// that showed up when this was cross-checked against the queue depth.
func (r *rig) admittedTotal() (int64, error) {
	snap, err := r.snapshot()
	if err != nil {
		return 0, err
	}
	return snap.TotalAdmitted, nil
}

func (r *rig) adminJSON(method, path string, body io.Reader, into any) error {
	req, err := http.NewRequest(method, r.admin()+"/__anteroom/admin/api"+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+r.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("%s %s: %s: %s", method, path, resp.Status, strings.TrimSpace(string(msg)))
	}
	if into == nil {
		io.Copy(io.Discard, resp.Body)
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(into)
}

// setHost aims the request at a host-matched room. Left empty the request
// carries the URL's own host, which lands in the catch-all.
func (r *rig) setHost(req *http.Request) {
	if r.host != "" {
		req.Host = r.host
	}
}

func pct(d []time.Duration, p float64) string {
	if len(d) == 0 {
		return "n/a"
	}
	i := int(float64(len(d)-1) * p)
	return d[i].Round(time.Microsecond * 100).String()
}

func mib(b int64) string {
	if b == 0 {
		return "n/a"
	}
	return fmt.Sprintf("%.0f MiB", float64(b)/(1<<20))
}

func commas(n int64) string {
	s := strconv.FormatInt(n, 10)
	if len(s) <= 3 {
		return s
	}
	var b strings.Builder
	pre := len(s) % 3
	if pre > 0 {
		b.WriteString(s[:pre])
	}
	for i := pre; i < len(s); i += 3 {
		if b.Len() > 0 {
			b.WriteByte(',')
		}
		b.WriteString(s[i : i+3])
	}
	return b.String()
}

func hostFacts() string {
	cpu, _ := exec.Command("sysctl", "-n", "machdep.cpu.brand_string").Output()
	cores, _ := exec.Command("sysctl", "-n", "hw.ncpu").Output()
	mem, _ := exec.Command("sysctl", "-n", "hw.memsize").Output()
	memGB := 0.0
	if n, err := strconv.ParseInt(strings.TrimSpace(string(mem)), 10, 64); err == nil {
		memGB = float64(n) / (1 << 30)
	}
	return fmt.Sprintf("%s, %s cores, %.0f GB",
		strings.TrimSpace(string(cpu)), strings.TrimSpace(string(cores)), memGB)
}
