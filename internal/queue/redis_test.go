package queue

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// fixture wires a store to an in-memory Redis with a clock the test drives.
type fixture struct {
	t     *testing.T
	store *RedisStore
	redis *miniredis.Miniredis
	clock time.Time
	ctx   context.Context
}

const testRoom = "shop"

func newFixture(t *testing.T) *fixture {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { rdb.Close() })

	f := &fixture{
		t:     t,
		store: NewRedisStore(rdb),
		redis: mr,
		clock: time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC),
		ctx:   context.Background(),
	}
	f.store.SetClock(func() time.Time { return f.clock })
	return f
}

// seed configures the room. Rate is per second; ttl and abandon in seconds.
func (f *fixture) seed(rate float64, maxActive int, ttl, abandon time.Duration) {
	f.t.Helper()
	cfg := RoomConfig{Rate: rate, MaxActive: maxActive, SessionTTL: ttl, AbandonAfter: abandon}
	if err := f.store.Seed(f.ctx, testRoom, cfg, true); err != nil {
		f.t.Fatalf("seed: %v", err)
	}
}

func (f *fixture) advance(d time.Duration) { f.clock = f.clock.Add(d) }

func (f *fixture) join(ids ...string) {
	f.t.Helper()
	for _, id := range ids {
		if _, err := f.store.Join(f.ctx, testRoom, id); err != nil {
			f.t.Fatalf("join %s: %v", id, err)
		}
	}
}

func (f *fixture) admit() AdmitResult {
	f.t.Helper()
	res, err := f.store.Admit(f.ctx, testRoom)
	if err != nil {
		f.t.Fatalf("admit: %v", err)
	}
	return res
}

func (f *fixture) snapshot() Snapshot {
	f.t.Helper()
	snap, err := f.store.Snapshot(f.ctx, testRoom)
	if err != nil {
		f.t.Fatalf("snapshot: %v", err)
	}
	return snap
}

func TestJoinReturnsPositionAndIsIdempotent(t *testing.T) {
	f := newFixture(t)
	f.seed(1, 10, time.Minute, time.Minute)

	for i, id := range []string{"a", "b", "c"} {
		pos, err := f.store.Join(f.ctx, testRoom, id)
		if err != nil {
			t.Fatal(err)
		}
		if want := int64(i + 1); pos != want {
			t.Errorf("Join(%s) = %d, want %d", id, pos, want)
		}
	}

	// Re-joining must not move the visitor to the back of the line.
	pos, err := f.store.Join(f.ctx, testRoom, "a")
	if err != nil {
		t.Fatal(err)
	}
	if pos != 1 {
		t.Errorf("re-Join(a) = %d, want 1", pos)
	}
	if got := f.snapshot(); got.Waiting != 3 || got.TotalJoined != 3 {
		t.Errorf("waiting=%d joined=%d, want 3 and 3", got.Waiting, got.TotalJoined)
	}
}

// resolve stands in for a visitor request from an address with no join limit
// applied; resolveFrom exercises the limit.
func (f *fixture) resolve(id string) Resolution {
	f.t.Helper()
	return f.resolveFrom(id, "")
}

func (f *fixture) resolveFrom(id, bucket string) Resolution {
	f.t.Helper()
	res, err := f.store.Resolve(f.ctx, testRoom, id, bucket)
	if err != nil {
		f.t.Fatalf("resolve %s: %v", id, err)
	}
	return res
}

func TestResolveQueuesNewVisitors(t *testing.T) {
	f := newFixture(t)
	f.seed(1, 10, time.Minute, time.Minute)

	first := f.resolve("a")
	if first.Admitted || first.Position != 1 || !first.Joined {
		t.Errorf("first resolve = %+v, want waiting at position 1, joined", first)
	}
	// Repeating the request keeps their place and does not re-announce them.
	again := f.resolve("a")
	if again.Admitted || again.Position != 1 || again.Joined {
		t.Errorf("repeat resolve = %+v, want position 1 without re-joining", again)
	}
	second := f.resolve("b")
	if second.Position != 2 || !second.Joined {
		t.Errorf("second visitor = %+v, want position 2, joined", second)
	}
	if got := f.snapshot().TotalJoined; got != 2 {
		t.Errorf("TotalJoined = %d, want 2", got)
	}
}

func TestResolveLetsAdmittedVisitorsThrough(t *testing.T) {
	f := newFixture(t)
	f.seed(100, 5, 30*time.Second, time.Hour)
	f.resolve("a")
	f.advance(time.Second)
	f.admit()

	// The visitor's next request finds them admitted rather than re-queued.
	res := f.resolve("a")
	if !res.Admitted || res.Position != 0 || res.Joined {
		t.Errorf("resolve after admission = %+v, want admitted", res)
	}
	// And each request refreshes the session, so browsing keeps it alive.
	for range 5 {
		f.advance(20 * time.Second)
		if got := f.resolve("a"); !got.Admitted {
			t.Fatalf("session died while the visitor was still browsing: %+v", got)
		}
	}
}

func TestResolveRequeuesExpiredSessions(t *testing.T) {
	f := newFixture(t)
	f.seed(100, 5, 30*time.Second, time.Hour)
	f.resolve("a")
	f.advance(time.Second)
	f.admit()

	f.advance(31 * time.Second)
	res := f.resolve("a")
	if res.Admitted {
		t.Error("an idle session was still treated as admitted")
	}
	if res.Position != 1 || !res.Joined {
		t.Errorf("resolve after expiry = %+v, want a fresh place in line", res)
	}
	if f.snapshot().Active != 0 {
		t.Error("the dead session was left in the active set")
	}
}

func TestResolveRefreshesHeartbeat(t *testing.T) {
	f := newFixture(t)
	f.seed(1, 100, time.Minute, 20*time.Second)
	f.resolve("stays")
	f.resolve("leaves")

	// "stays" keeps requesting the waiting page; "leaves" goes quiet.
	f.advance(15 * time.Second)
	f.resolve("stays")

	f.advance(10 * time.Second)
	res := f.admit()
	if !slices.Equal(res.Abandoned, []string{"leaves"}) {
		t.Errorf("abandoned %v, want [leaves]", res.Abandoned)
	}
	if !slices.Equal(res.Admitted, []string{"stays"}) {
		t.Errorf("admitted %v, want [stays]", res.Admitted)
	}
}

// seedJoinLimit configures a room with a per-address join limit.
func (f *fixture) seedJoinLimit(limit int, window time.Duration) {
	f.t.Helper()
	cfg := RoomConfig{
		Rate: 1, MaxActive: 100, SessionTTL: time.Minute, AbandonAfter: time.Hour,
		JoinLimit: limit, JoinWindow: window,
	}
	if err := f.store.Seed(f.ctx, testRoom, cfg, true); err != nil {
		f.t.Fatalf("seed: %v", err)
	}
}

func TestJoinLimitRefusesAfterTheLimit(t *testing.T) {
	f := newFixture(t)
	f.seedJoinLimit(3, time.Minute)

	for i := range 3 {
		res := f.resolveFrom(fmt.Sprintf("v%d", i), "198.51.100.4")
		if res.Refused {
			t.Fatalf("visitor %d was refused while still inside the limit", i)
		}
		if !res.Joined {
			t.Fatalf("visitor %d did not join", i)
		}
	}

	res := f.resolveFrom("one-too-many", "198.51.100.4")
	if !res.Refused {
		t.Error("the fourth visitor from one address was not refused")
	}
	if res.Joined || res.Position != 0 {
		t.Errorf("a refused visitor was still queued: %+v", res)
	}
	if snap := f.snapshot(); snap.Waiting != 3 || snap.TotalRefused != 1 {
		t.Errorf("waiting=%d refused=%d, want 3 and 1", snap.Waiting, snap.TotalRefused)
	}
}

func TestJoinLimitDoesNotChargeReturningVisitors(t *testing.T) {
	// A visitor already in line who reloads, or whose stream reconnects, must
	// never spend budget: an impatient person would otherwise throttle
	// themselves out of the place they already hold.
	f := newFixture(t)
	f.seedJoinLimit(2, time.Minute)

	first := f.resolveFrom("a", "198.51.100.4")
	if !first.Joined {
		t.Fatal("first visitor did not join")
	}
	for range 20 {
		res := f.resolveFrom("a", "198.51.100.4")
		if res.Refused {
			t.Fatal("a visitor already in line was refused on reload")
		}
		if res.Position != 1 {
			t.Fatalf("position = %d, want 1", res.Position)
		}
	}
	// The budget is untouched, so a genuine second visitor still gets in.
	if res := f.resolveFrom("b", "198.51.100.4"); res.Refused {
		t.Error("reloads consumed the budget of a second real visitor")
	}
}

func TestJoinLimitCountsRefusalsSoHammeringCannotResetTheWindow(t *testing.T) {
	f := newFixture(t)
	f.seedJoinLimit(1, time.Minute)
	f.resolveFrom("a", "198.51.100.4")

	// Keep knocking. Every attempt is counted, so the window never rolls over
	// just because the caller stopped succeeding.
	for i := range 10 {
		if res := f.resolveFrom(fmt.Sprintf("bot%d", i), "198.51.100.4"); !res.Refused {
			t.Fatalf("attempt %d got in past the limit", i)
		}
	}
	if snap := f.snapshot(); snap.TotalRefused != 10 {
		t.Errorf("TotalRefused = %d, want 10", snap.TotalRefused)
	}
}

func TestJoinLimitWindowExpires(t *testing.T) {
	f := newFixture(t)
	f.seedJoinLimit(1, 30*time.Second)
	f.resolveFrom("a", "198.51.100.4")
	if res := f.resolveFrom("b", "198.51.100.4"); !res.Refused {
		t.Fatal("second visitor was not refused inside the window")
	}

	// miniredis only expires keys when its own clock is advanced.
	f.redis.FastForward(31 * time.Second)

	if res := f.resolveFrom("c", "198.51.100.4"); res.Refused {
		t.Error("the budget did not recover after the window elapsed")
	}
}

func TestJoinLimitIsPerAddress(t *testing.T) {
	f := newFixture(t)
	f.seedJoinLimit(1, time.Minute)

	if res := f.resolveFrom("a", "198.51.100.4"); res.Refused {
		t.Fatal("first address was refused")
	}
	if res := f.resolveFrom("b", "198.51.100.5"); res.Refused {
		t.Error("a different address was refused on the first address's budget")
	}
}

func TestJoinLimitSkippedWithoutABucket(t *testing.T) {
	// When the client address cannot be determined there is nobody to count
	// against, and lumping everyone into one bucket would throttle the world.
	f := newFixture(t)
	f.seedJoinLimit(1, time.Minute)
	for i := range 5 {
		if res := f.resolveFrom(fmt.Sprintf("v%d", i), ""); res.Refused {
			t.Fatalf("visitor %d was refused with no bucket to count against", i)
		}
	}
}

func TestJoinLimitDisabledByZero(t *testing.T) {
	f := newFixture(t)
	f.seedJoinLimit(0, time.Minute)
	for i := range 10 {
		if res := f.resolveFrom(fmt.Sprintf("v%d", i), "198.51.100.4"); res.Refused {
			t.Fatalf("visitor %d was refused though the limit is disabled", i)
		}
	}
}

// seedScheduled configures a room whose doors open at admitsAt, collecting
// visitors from opensAt onwards.
func (f *fixture) seedScheduled(opensAt, admitsAt, closesAt time.Time, lottery bool) {
	f.t.Helper()
	// Sessions and heartbeats outlast the schedule being tested, so that
	// jumping the clock across a window does not expire or abandon anyone;
	// those behaviours have their own tests.
	cfg := RoomConfig{
		Rate: 100, MaxActive: 1000, SessionTTL: time.Hour, AbandonAfter: 24 * time.Hour,
		Lottery: lottery, DrawSalt: "test-salt",
		QueueOpensAt: opensAt, AdmitsAt: admitsAt, ClosesAt: closesAt,
	}
	if err := f.store.Seed(f.ctx, testRoom, cfg, true); err != nil {
		f.t.Fatalf("seed: %v", err)
	}
}

func TestScheduleHoldsVisitorsBeforeTheQueueOpens(t *testing.T) {
	f := newFixture(t)
	opens := f.clock.Add(10 * time.Minute)
	f.seedScheduled(opens, opens.Add(30*time.Minute), time.Time{}, true)

	res := f.resolve("early-bird")
	if res.Phase != PhaseBefore {
		t.Errorf("phase = %v, want before", res.Phase)
	}
	if res.Joined || res.Position != 0 {
		t.Errorf("a visitor was queued before the queue opened: %+v", res)
	}
	if snap := f.snapshot(); snap.Waiting != 0 {
		t.Errorf("waiting = %d before opening, want 0", snap.Waiting)
	}
}

func TestScheduleCollectsDuringTheDrawThenAdmits(t *testing.T) {
	f := newFixture(t)
	opens := f.clock
	admits := f.clock.Add(30 * time.Minute)
	f.seedScheduled(opens, admits, time.Time{}, true)

	for i := range 5 {
		res := f.resolve(fmt.Sprintf("v%d", i))
		if res.Phase != PhaseDraw {
			t.Fatalf("phase = %v, want draw", res.Phase)
		}
		if !res.Joined {
			t.Fatalf("visitor %d was not collected", i)
		}
	}
	// Collected, but the doors are shut.
	f.advance(time.Minute)
	if got := f.admit(); len(got.Admitted) != 0 {
		t.Errorf("admitted %d during the draw window, want 0", len(got.Admitted))
	}
	if snap := f.snapshot(); snap.Waiting != 5 || snap.Phase != PhaseDraw {
		t.Errorf("waiting=%d phase=%v, want 5 and draw", snap.Waiting, snap.Phase)
	}

	// Doors open.
	f.advance(30 * time.Minute)
	if got := f.admit(); len(got.Admitted) != 5 {
		t.Errorf("admitted %d after the doors opened, want 5", len(got.Admitted))
	}
	if snap := f.snapshot(); snap.Phase != PhaseQueueing {
		t.Errorf("phase = %v after opening, want queueing", snap.Phase)
	}
}

func TestScheduleStopsAdmittingAfterClosing(t *testing.T) {
	f := newFixture(t)
	closes := f.clock.Add(10 * time.Minute)
	f.seedScheduled(time.Time{}, time.Time{}, closes, false)

	f.resolve("a")
	f.advance(time.Second)
	if got := f.admit(); len(got.Admitted) != 1 {
		t.Fatalf("admitted %d before closing, want 1", len(got.Admitted))
	}

	f.advance(11 * time.Minute)
	f.resolve("b")
	if got := f.admit(); len(got.Admitted) != 0 {
		t.Errorf("admitted %d after closing, want 0", len(got.Admitted))
	}
	if res := f.resolve("c"); res.Phase != PhaseClosed || res.Joined {
		t.Errorf("a closed room still queued a visitor: %+v", res)
	}
}

func TestClosingDoesNotEvictVisitorsAlreadyOnTheSite(t *testing.T) {
	// Closing means no new admissions, not throwing people out mid-purchase.
	f := newFixture(t)
	closes := f.clock.Add(10 * time.Minute)
	f.seedScheduled(time.Time{}, time.Time{}, closes, false)

	f.resolve("shopper")
	f.advance(time.Second)
	f.admit()

	f.advance(11 * time.Minute)
	if res := f.resolve("shopper"); !res.Admitted {
		t.Errorf("an admitted visitor lost their session when the room closed: %+v", res)
	}
}

func TestLotteryPlaceSurvivesRejoining(t *testing.T) {
	// The property that makes the draw worth having: leaving and coming back
	// must not reroll your place, or a bot would do it continuously until it
	// drew a good one.
	f := newFixture(t)
	f.seedScheduled(f.clock, f.clock.Add(time.Hour), time.Time{}, true)

	for i := range 20 {
		f.resolve(fmt.Sprintf("other%d", i))
	}
	first := f.resolve("roller").Position

	for range 10 {
		// Abandon and come back.
		if _, err := f.store.Flush(f.ctx, testRoom); err == nil {
			// Flush clears everyone, so rebuild the same crowd to compare
			// against the same field.
			for i := range 20 {
				f.resolve(fmt.Sprintf("other%d", i))
			}
		}
		if got := f.resolve("roller").Position; got != first {
			t.Fatalf("rejoining moved the visitor from %d to %d", first, got)
		}
	}
}

func TestLotteryOrderIgnoresArrivalOrder(t *testing.T) {
	// If the draw simply preserved arrival order it would reward camping.
	f := newFixture(t)
	f.seedScheduled(f.clock, f.clock.Add(time.Hour), time.Time{}, true)

	const n = 60
	var arrival []string
	for i := range n {
		id := fmt.Sprintf("visitor-%02d", i)
		arrival = append(arrival, id)
		f.resolve(id)
	}

	f.advance(time.Hour + time.Minute)
	admitted := f.admit().Admitted
	if len(admitted) != n {
		t.Fatalf("admitted %d, want %d", len(admitted), n)
	}
	if slices.Equal(admitted, arrival) {
		t.Error("the draw handed back arrival order, so arriving early still pays")
	}

	// Everyone still gets in exactly once.
	seen := map[string]bool{}
	for _, id := range admitted {
		if seen[id] {
			t.Fatalf("%s was admitted twice", id)
		}
		seen[id] = true
	}
	if len(seen) != n {
		t.Errorf("%d distinct visitors admitted, want %d", len(seen), n)
	}
}

func TestLotteryEntriesGoBeforeLaterArrivals(t *testing.T) {
	// Draw places sit in [0,1) and later arrivals get sequence numbers from 1
	// up, so everyone in the draw is served before anyone who turned up after
	// the doors opened.
	f := newFixture(t)
	admits := f.clock.Add(30 * time.Minute)
	f.seedScheduled(f.clock, admits, time.Time{}, true)

	for i := range 10 {
		f.resolve(fmt.Sprintf("draw-%d", i))
	}
	f.advance(31 * time.Minute)
	for i := range 5 {
		f.resolve(fmt.Sprintf("late-%d", i))
	}

	admitted := f.admit().Admitted
	if len(admitted) != 15 {
		t.Fatalf("admitted %d, want 15", len(admitted))
	}
	for i, id := range admitted[:10] {
		if !strings.HasPrefix(id, "draw-") {
			t.Errorf("position %d went to %q, want a draw entry", i+1, id)
		}
	}
	for i, id := range admitted[10:] {
		if !strings.HasPrefix(id, "late-") {
			t.Errorf("position %d went to %q, want a late arrival", i+11, id)
		}
	}
}

func TestLotteryOffKeepsArrivalOrder(t *testing.T) {
	// A scheduled room without the draw is still plain FIFO.
	f := newFixture(t)
	f.seedScheduled(f.clock, f.clock.Add(30*time.Minute), time.Time{}, false)

	var ids []string
	for i := range 8 {
		id := fmt.Sprintf("v%d", i)
		ids = append(ids, id)
		f.resolve(id)
	}
	f.advance(31 * time.Minute)
	if got := f.admit().Admitted; !slices.Equal(got, ids) {
		t.Errorf("admitted %v, want arrival order %v", got, ids)
	}
}

func TestUnscheduledRoomIsAlwaysQueueing(t *testing.T) {
	f := newFixture(t)
	f.seed(1, 10, time.Minute, time.Hour)
	if res := f.resolve("a"); res.Phase != PhaseQueueing {
		t.Errorf("phase = %v, want queueing", res.Phase)
	}
	if snap := f.snapshot(); snap.Phase != PhaseQueueing {
		t.Errorf("snapshot phase = %v, want queueing", snap.Phase)
	}
}

func TestPositionZeroWhenNotWaiting(t *testing.T) {
	f := newFixture(t)
	f.seed(1, 10, time.Minute, time.Minute)
	f.join("a")

	if pos, _ := f.store.Position(f.ctx, testRoom, "a"); pos != 1 {
		t.Errorf("Position(a) = %d, want 1", pos)
	}
	if pos, _ := f.store.Position(f.ctx, testRoom, "stranger"); pos != 0 {
		t.Errorf("Position(stranger) = %d, want 0", pos)
	}
}

func TestAdmitIsFIFO(t *testing.T) {
	f := newFixture(t)
	// One admit per second, generous cap: order is the only thing under test.
	f.seed(1, 100, time.Minute, time.Minute)

	var ids []string
	for i := range 10 {
		ids = append(ids, fmt.Sprintf("v%02d", i))
	}
	f.join(ids...)

	var got []string
	for range 10 {
		f.advance(time.Second)
		got = append(got, f.admit().Admitted...)
	}
	if !slices.Equal(got, ids) {
		t.Errorf("admitted %v, want join order %v", got, ids)
	}
}

func TestAdmitRespectsRate(t *testing.T) {
	f := newFixture(t)
	// A long abandon window keeps the idle gap below from reaping the queue;
	// abandonment has its own test.
	f.seed(2, 100, time.Minute, time.Hour) // 2/sec
	for i := range 20 {
		f.join(fmt.Sprintf("v%02d", i))
	}

	// The bucket starts empty, so the first pass admits nothing.
	if got := f.admit(); len(got.Admitted) != 0 {
		t.Errorf("first pass admitted %d, want 0 (bucket starts empty)", len(got.Admitted))
	}
	// A quarter-second accrues half a token: still nothing whole to spend.
	f.advance(250 * time.Millisecond)
	if got := f.admit(); len(got.Admitted) != 0 {
		t.Errorf("admitted %d after 250ms at 2/s, want 0", len(got.Admitted))
	}
	// A full second from the start is 2 tokens.
	f.advance(750 * time.Millisecond)
	if got := f.admit(); len(got.Admitted) != 2 {
		t.Errorf("admitted %d after 1s at 2/s, want 2", len(got.Admitted))
	}
	// Burst is capped at one second of rate, so a long idle gap does not
	// dump the entire queue on the origin.
	f.advance(time.Minute)
	if got := f.admit(); len(got.Admitted) != 2 {
		t.Errorf("admitted %d after a 60s gap, want 2 (burst cap)", len(got.Admitted))
	}
}

func TestFractionalRateAccrues(t *testing.T) {
	f := newFixture(t)
	f.seed(0.5, 100, time.Minute, time.Minute) // one admit every 2s
	f.join("a", "b")

	f.advance(time.Second)
	if got := f.admit(); len(got.Admitted) != 0 {
		t.Errorf("admitted %d after 1s at 0.5/s, want 0", len(got.Admitted))
	}
	f.advance(time.Second)
	if got := f.admit(); len(got.Admitted) != 1 {
		t.Errorf("admitted %d after 2s at 0.5/s, want 1", len(got.Admitted))
	}
}

func TestAdmitRespectsConcurrencyCap(t *testing.T) {
	f := newFixture(t)
	// Plenty of rate, but only 3 may be on the site at once.
	f.seed(100, 3, time.Minute, time.Minute)
	for i := range 10 {
		f.join(fmt.Sprintf("v%02d", i))
	}

	f.advance(time.Second)
	if got := f.admit(); len(got.Admitted) != 3 {
		t.Fatalf("admitted %d, want 3 (cap)", len(got.Admitted))
	}
	f.advance(time.Second)
	if got := f.admit(); len(got.Admitted) != 0 {
		t.Errorf("admitted %d while at cap, want 0", len(got.Admitted))
	}
	if snap := f.snapshot(); snap.Active != 3 || snap.Waiting != 7 {
		t.Errorf("active=%d waiting=%d, want 3 and 7", snap.Active, snap.Waiting)
	}
}

func TestExpiredSessionsFreeCapacity(t *testing.T) {
	f := newFixture(t)
	f.seed(100, 2, 30*time.Second, time.Hour)
	f.join("a", "b", "c")

	f.advance(time.Second)
	first := f.admit().Admitted
	if len(first) != 2 {
		t.Fatalf("admitted %d, want 2", len(first))
	}

	// Nobody touches their session, so both expire and free their slots.
	f.advance(31 * time.Second)
	res := f.admit()
	if !slices.Equal(res.Expired, first) {
		t.Errorf("expired %v, want %v", res.Expired, first)
	}
	if !slices.Equal(res.Admitted, []string{"c"}) {
		t.Errorf("admitted %v, want [c]", res.Admitted)
	}
	if snap := f.snapshot(); snap.TotalExpired != 2 {
		t.Errorf("TotalExpired = %d, want 2", snap.TotalExpired)
	}
}

func TestTouchKeepsSessionAlive(t *testing.T) {
	f := newFixture(t)
	f.seed(100, 5, 30*time.Second, time.Hour)
	f.join("a")
	f.advance(time.Second)
	f.admit()

	// Touching every 20s keeps a 30s session alive indefinitely.
	for range 5 {
		f.advance(20 * time.Second)
		alive, err := f.store.Touch(f.ctx, testRoom, "a")
		if err != nil {
			t.Fatal(err)
		}
		if !alive {
			t.Fatal("Touch reported a touched session as dead")
		}
	}

	// Stop touching and it dies.
	f.advance(31 * time.Second)
	if alive, _ := f.store.Touch(f.ctx, testRoom, "a"); alive {
		t.Error("Touch reported an idle session as alive")
	}
	// A visitor the store never saw is likewise not alive.
	if alive, _ := f.store.Touch(f.ctx, testRoom, "stranger"); alive {
		t.Error("Touch reported an unknown visitor as alive")
	}
}

func TestAbandonedWaitersAreReaped(t *testing.T) {
	f := newFixture(t)
	f.seed(1, 100, time.Minute, 20*time.Second)
	f.join("stays", "leaves")

	// "stays" keeps its page open; "leaves" closes the tab.
	f.advance(15 * time.Second)
	if err := f.store.Heartbeat(f.ctx, testRoom, "stays"); err != nil {
		t.Fatal(err)
	}

	f.advance(10 * time.Second) // 25s since "leaves" was last seen
	res := f.admit()
	if !slices.Equal(res.Abandoned, []string{"leaves"}) {
		t.Errorf("abandoned %v, want [leaves]", res.Abandoned)
	}
	if !slices.Equal(res.Admitted, []string{"stays"}) {
		t.Errorf("admitted %v, want [stays]", res.Admitted)
	}
	if snap := f.snapshot(); snap.TotalAbandoned != 1 || snap.Waiting != 0 {
		t.Errorf("abandoned=%d waiting=%d, want 1 and 0", snap.TotalAbandoned, snap.Waiting)
	}
}

func TestHeartbeatDoesNotResurrectAdmittedVisitors(t *testing.T) {
	f := newFixture(t)
	f.seed(100, 5, time.Minute, 20*time.Second)
	f.join("a")
	f.advance(time.Second)
	f.admit()

	if err := f.store.Heartbeat(f.ctx, testRoom, "a"); err != nil {
		t.Fatal(err)
	}
	// An admitted visitor left `seen`, so a stray heartbeat must not put them
	// back into the abandonment set (where a later reap would "abandon" them).
	f.advance(time.Minute)
	if res := f.admit(); len(res.Abandoned) != 0 {
		t.Errorf("abandoned %v, want none", res.Abandoned)
	}
}

func TestPauseStopsAdmissions(t *testing.T) {
	f := newFixture(t)
	f.seed(5, 100, time.Minute, time.Hour)
	f.join("a", "b", "c")

	if err := f.store.SetPaused(f.ctx, testRoom, true); err != nil {
		t.Fatal(err)
	}
	f.advance(time.Second)
	if res := f.admit(); len(res.Admitted) != 0 {
		t.Errorf("admitted %d while paused, want 0", len(res.Admitted))
	}
	if !f.snapshot().Paused {
		t.Error("snapshot does not report the room as paused")
	}

	if err := f.store.SetPaused(f.ctx, testRoom, false); err != nil {
		t.Fatal(err)
	}
	if res := f.admit(); len(res.Admitted) != 3 {
		t.Errorf("admitted %d after resume, want 3", len(res.Admitted))
	}
}

func TestPauseStillExpiresAndReaps(t *testing.T) {
	// Pausing admissions must not stall housekeeping, or a paused room would
	// accumulate dead sessions and ghost waiters.
	f := newFixture(t)
	f.seed(100, 5, 30*time.Second, 20*time.Second)
	f.join("admitted-one")
	f.advance(time.Second)
	f.admit()
	f.join("abandoner")

	if err := f.store.SetPaused(f.ctx, testRoom, true); err != nil {
		t.Fatal(err)
	}
	f.advance(time.Minute)
	res := f.admit()
	if !slices.Equal(res.Expired, []string{"admitted-one"}) {
		t.Errorf("expired %v, want [admitted-one]", res.Expired)
	}
	if !slices.Equal(res.Abandoned, []string{"abandoner"}) {
		t.Errorf("abandoned %v, want [abandoner]", res.Abandoned)
	}
}

func TestFlushClearsQueueButKeepsSessions(t *testing.T) {
	f := newFixture(t)
	f.seed(100, 5, time.Minute, time.Minute)
	f.join("admitted-one")
	f.advance(time.Second)
	f.admit()
	f.join("w1", "w2", "w3")

	removed, err := f.store.Flush(f.ctx, testRoom)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 3 {
		t.Errorf("Flush removed %d, want 3", removed)
	}
	snap := f.snapshot()
	if snap.Waiting != 0 {
		t.Errorf("waiting = %d after flush, want 0", snap.Waiting)
	}
	if snap.Active != 1 {
		t.Errorf("active = %d after flush, want 1 (sessions survive a flush)", snap.Active)
	}
}

func TestSeedPreservesRuntimeChanges(t *testing.T) {
	f := newFixture(t)
	original := RoomConfig{Rate: 5, MaxActive: 10, SessionTTL: time.Minute, AbandonAfter: time.Minute}
	if err := f.store.Seed(f.ctx, testRoom, original, false); err != nil {
		t.Fatal(err)
	}

	// An operator raises the rate through the admin API.
	tuned := original
	tuned.Rate = 99
	if err := f.store.SetConfig(f.ctx, testRoom, tuned); err != nil {
		t.Fatal(err)
	}

	// A restart re-seeds from the config file and must not clobber that.
	if err := f.store.Seed(f.ctx, testRoom, original, false); err != nil {
		t.Fatal(err)
	}
	if got := f.snapshot().Rate; got != 99 {
		t.Errorf("rate = %v after restart, want the runtime value 99", got)
	}

	// Unless the operator explicitly asks to reseed from the file.
	if err := f.store.Seed(f.ctx, testRoom, original, true); err != nil {
		t.Fatal(err)
	}
	if got := f.snapshot().Rate; got != 5 {
		t.Errorf("rate = %v after overwrite seed, want the file value 5", got)
	}
}

func TestSetConfigLeavesPauseAlone(t *testing.T) {
	f := newFixture(t)
	f.seed(1, 10, time.Minute, time.Minute)
	if err := f.store.SetPaused(f.ctx, testRoom, true); err != nil {
		t.Fatal(err)
	}
	cfg := RoomConfig{Rate: 7, MaxActive: 20, SessionTTL: time.Minute, AbandonAfter: time.Minute}
	if err := f.store.SetConfig(f.ctx, testRoom, cfg); err != nil {
		t.Fatal(err)
	}
	snap := f.snapshot()
	if !snap.Paused {
		t.Error("SetConfig cleared the paused flag")
	}
	if snap.Rate != 7 || snap.MaxActive != 20 {
		t.Errorf("rate=%v cap=%d, want 7 and 20", snap.Rate, snap.MaxActive)
	}
}

func TestRoomsAreIsolated(t *testing.T) {
	f := newFixture(t)
	f.seed(100, 100, time.Minute, time.Minute) // shop
	other := RoomConfig{Rate: 100, MaxActive: 100, SessionTTL: time.Minute, AbandonAfter: time.Minute}
	if err := f.store.Seed(f.ctx, "tickets", other, true); err != nil {
		t.Fatal(err)
	}

	f.join("shopper")
	if _, err := f.store.Join(f.ctx, "tickets", "fan"); err != nil {
		t.Fatal(err)
	}
	// The same visitor ID in two rooms is two independent queue entries.
	if _, err := f.store.Join(f.ctx, "tickets", "shopper"); err != nil {
		t.Fatal(err)
	}

	f.advance(time.Second)
	if got := f.admit().Admitted; !slices.Equal(got, []string{"shopper"}) {
		t.Errorf("shop admitted %v, want [shopper]", got)
	}
	ticketSnap, err := f.store.Snapshot(f.ctx, "tickets")
	if err != nil {
		t.Fatal(err)
	}
	if ticketSnap.Waiting != 2 {
		t.Errorf("tickets waiting = %d, want 2 (untouched by the shop admit)", ticketSnap.Waiting)
	}
	// Being admitted to the shop grants nothing in tickets.
	if alive, _ := f.store.Touch(f.ctx, "tickets", "shopper"); alive {
		t.Error("a shop session was accepted as a tickets session")
	}
}

func TestConcurrentAdmitPassesNeverDoubleAdmit(t *testing.T) {
	// Two replicas ticking at the same instant share one bucket and one queue,
	// so the second pass must find the tokens already spent.
	f := newFixture(t)
	f.seed(2, 100, time.Minute, time.Minute)
	f.join("a", "b", "c", "d")

	f.advance(time.Second)
	first := f.admit().Admitted
	second := f.admit().Admitted // same instant, no new tokens

	if len(first) != 2 {
		t.Errorf("first pass admitted %d, want 2", len(first))
	}
	if len(second) != 0 {
		t.Errorf("second pass at the same instant admitted %d, want 0", len(second))
	}
	for _, id := range second {
		if slices.Contains(first, id) {
			t.Errorf("visitor %s was admitted twice", id)
		}
	}
}

func TestEmptyQueueAdmitIsHarmless(t *testing.T) {
	f := newFixture(t)
	f.seed(10, 10, time.Minute, time.Minute)
	f.advance(time.Second)
	res := f.admit()
	if !res.empty() {
		t.Errorf("admit on an empty room returned %+v, want nothing", res)
	}
}

func TestSnapshotReportsSettings(t *testing.T) {
	f := newFixture(t)
	f.seed(2.5, 42, 90*time.Second, 30*time.Second)
	snap := f.snapshot()
	if snap.Room != testRoom || snap.Rate != 2.5 || snap.MaxActive != 42 ||
		snap.SessionTTLSecs != 90 || snap.AbandonAfterSecs != 30 {
		t.Errorf("snapshot = %+v", snap)
	}
}

func TestAdmitLargeBatchChunksRemovals(t *testing.T) {
	// Exercises the chunked ZREM path: more members than one unpack() can take.
	f := newFixture(t)
	f.seed(2000, 5000, time.Hour, 10*time.Second)
	for i := range 1000 {
		f.join(fmt.Sprintf("v%04d", i))
	}
	f.advance(time.Second)
	if got := len(f.admit().Admitted); got != 1000 {
		t.Fatalf("admitted %d, want 1000", got)
	}
	if snap := f.snapshot(); snap.Waiting != 0 || snap.Active != 1000 {
		t.Errorf("waiting=%d active=%d, want 0 and 1000", snap.Waiting, snap.Active)
	}
}
