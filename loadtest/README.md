# loadtest

Measures what one anteroom actually holds, so the numbers in the README are
measurements rather than opinions.

This is its own Go module on purpose. It needs a running anteroom, a real
Redis and several minutes, none of which belong in `make check`, and keeping
it separate means `go test ./...` in the repository above never picks it up.

## Running it

Start a real Redis (miniredis is fine for the unit tests, but it tells you
nothing about memory or throughput):

```sh
docker run -d --name anteroom-loadtest-redis -p 6379:6379 \
  redis:7-alpine redis-server --save "" --appendonly no
```

Then an origin to sit behind, an anteroom, and the test:

```sh
python3 -m http.server 9999 --bind 127.0.0.1 &
make build
./bin/anteroom --config loadtest/anteroom.loadtest.yaml &

cd loadtest && go build -o loadtest .
./loadtest -fill 1000000 -workers 512 \
  -pid "$(pgrep -f 'bin/anteroom --config loadtest')" \
  -redis 127.0.0.1:6379 -drain-rate 20000 -drain-for 20s
```

`anteroom.loadtest.yaml` lifts the per-address join limit, the idle timeout
and the concurrency cap out of the way. Every visitor arrives from 127.0.0.1
and none of them send a heartbeat, so with a normal configuration the run
would refuse itself after 120 joins and then abandon everyone.

## What the phases do

**Fill** joins visitors as fast as the workers can, with admissions paused so
the depth reached is the depth measured. It reports throughput and the
latency an arriving visitor sees.

**Hold** opens one position stream per visitor and keeps it open, which is
what a browser sitting on the waiting page does. This is the phase that finds
the real ceiling: every waiting visitor costs a socket, a goroutine and a
Redis read every couple of seconds. Streams are opened over `-ramp` rather
than all at once, because ten thousand simultaneous dials overrun the listen
backlog and measure the accept queue instead of what the server can hold.

**Drain** sets a rate, then reads the admission counter either side of a
window. It also reads the queue depth either side, because a counter can be
read wrong and the queue shrinking by the same number is the check on it.
That check earned its place: it caught the drain reading its counter from the
metrics endpoint, which answers from the one-second statistics cache. That is
right for a scraper and wrong for a delta, since at twenty thousand
admissions a second a cache one second stale is twenty thousand admissions of
error. It reads Redis live now.

## Driving replicas

Pass several comma-separated URLs and requests round-robin across them, which
is how the shared rate budget gets tested:

```sh
for port in 8099 8100 8101; do
  ./bin/anteroom --config loadtest/anteroom.loadtest.yaml --listen "127.0.0.1:$port" &
done

./loadtest -url http://127.0.0.1:8099,http://127.0.0.1:8100,http://127.0.0.1:8101 \
  -fill 200000 -drain-rate 1000 -drain-for 60s
```

Three replicas admitting at a configured 1,000 a second should admit about
1,000 a second in total, not 3,000. That is the whole point of admission
being one Lua script.

## Reading the results

`-json results.json` writes everything the run measured. Two things are worth
knowing before quoting any of it.

The client is a bottleneck long before anteroom is. macOS caps a process at
10,240 descriptors (`kern.maxfilesperproc`) and hands out 16,384 ephemeral
ports, so a single machine cannot open much more than nine or ten thousand
concurrent streams no matter how many replicas it talks to. Past that the
generator fails, not the server: anteroom logs `too many open files` and
carries on. Numbers above that need more than one machine.

Loopback also flatters the latencies, because there is no network between the
load generator, anteroom and Redis. Treat the throughput and memory figures
as real and the latencies as a floor.
