package queue

import "github.com/redis/go-redis/v9"

// Shared Lua prelude. Redis runs Lua 5.1, where unpack() is a global and is
// limited by the stack size, so bulk removals are chunked.
const luaPrelude = `
local function chunkedZRem(key, members)
  for i = 1, #members, 400 do
    local chunk = {}
    for j = i, math.min(i + 399, #members) do chunk[#chunk + 1] = members[j] end
    redis.call('ZREM', key, unpack(chunk))
  end
end

local function readConf(key)
  local flat = redis.call('HGETALL', key)
  local c = {}
  for i = 1, #flat, 2 do c[flat[i]] = flat[i + 1] end
  return {
    rate    = tonumber(c['rate']) or 0,
    cap     = tonumber(c['cap']) or 0,
    ttl     = tonumber(c['ttl_secs']) or 0,
    abandon = tonumber(c['abandon_secs']) or 0,
    paused  = c['paused'] == '1',
  }
end
`

// joinScript enqueues a visitor if they are not already waiting and returns
// their 1-based position. Re-joining is a no-op that only refreshes the
// heartbeat, so a reloaded queue page never loses its place.
//
//	KEYS: seq, waiting, seen, stats
//	ARGV: id, now_seconds
var joinScript = redis.NewScript(`
local id    = ARGV[1]
local now_s = tonumber(ARGV[2])

local rank = redis.call('ZRANK', KEYS[2], id)
if not rank then
  local seq = redis.call('INCR', KEYS[1])
  redis.call('ZADD', KEYS[2], seq, id)
  redis.call('HINCRBY', KEYS[4], 'joined', 1)
  rank = redis.call('ZRANK', KEYS[2], id)
end
redis.call('ZADD', KEYS[3], now_s, id)
return rank + 1
`)

// admitScript is one full admission pass, run by the dispatcher. Doing the
// whole pass in Lua keeps it atomic: two replicas ticking at the same instant
// can never admit the same visitor twice or jointly exceed the rate, because
// the token bucket and the queue are read and written in a single step.
//
//	KEYS: waiting, seen, active, bucket, conf, stats
//	ARGV: now_millis
//	Returns: {abandoned[], expired[], admitted[]}
var admitScript = redis.NewScript(luaPrelude + `
local now_ms = tonumber(ARGV[1])
local now_s  = now_ms / 1000
local conf   = readConf(KEYS[5])

-- Reap waiting visitors who stopped sending heartbeats. Done before admitting
-- so their slots go to visitors who are actually still there.
local abandoned = {}
if conf.abandon > 0 then
  abandoned = redis.call('ZRANGEBYSCORE', KEYS[2], '-inf', now_s - conf.abandon, 'LIMIT', 0, 1000)
  if #abandoned > 0 then
    chunkedZRem(KEYS[1], abandoned)
    chunkedZRem(KEYS[2], abandoned)
    redis.call('HINCRBY', KEYS[6], 'abandoned', #abandoned)
  end
end

-- Expire idle sessions, freeing capacity.
local expired = {}
if conf.ttl > 0 then
  expired = redis.call('ZRANGEBYSCORE', KEYS[3], '-inf', now_s - conf.ttl, 'LIMIT', 0, 1000)
  if #expired > 0 then
    chunkedZRem(KEYS[3], expired)
    redis.call('HINCRBY', KEYS[6], 'expired', #expired)
  end
end

-- Refill the token bucket. Burst is capped at one second of rate (minimum one
-- token) so a long pause or outage cannot dump the whole queue on the origin.
local tokens = tonumber(redis.call('HGET', KEYS[4], 'tokens')) or 0
local last   = tonumber(redis.call('HGET', KEYS[4], 'last')) or now_ms
local burst  = math.max(conf.rate, 1)
tokens = math.min(burst, tokens + conf.rate * math.max(0, (now_ms - last) / 1000))

local admitted = {}
if not conf.paused then
  local n = math.floor(tokens)
  local free = conf.cap - redis.call('ZCARD', KEYS[3])
  if n > free then n = free end
  if n > 0 then
    -- ZPOPMIN takes the lowest sequence numbers: strict FIFO.
    local popped = redis.call('ZPOPMIN', KEYS[1], n)
    for i = 1, #popped, 2 do
      admitted[#admitted + 1] = popped[i]
      redis.call('ZADD', KEYS[3], now_s, popped[i])
    end
    if #admitted > 0 then
      chunkedZRem(KEYS[2], admitted)
      redis.call('HINCRBY', KEYS[6], 'admitted', #admitted)
      tokens = tokens - #admitted
    end
  end
end

redis.call('HSET', KEYS[4], 'tokens', tostring(tokens), 'last', tostring(now_ms))
return {abandoned, expired, admitted}
`)

// resolveScript is the visitor request path in a single round trip: refresh a
// live session, or queue the visitor and report their position. Doing both in
// one atomic step means a visitor admitted between two of their own requests
// is never bounced back into the queue by a race.
//
//	KEYS: waiting, seen, active, conf, seq, stats
//	ARGV: id, now_millis
//	Returns: {admitted, position, joined}
var resolveScript = redis.NewScript(`
local id     = ARGV[1]
local now_s  = tonumber(ARGV[2]) / 1000
local ttl    = tonumber(redis.call('HGET', KEYS[4], 'ttl_secs')) or 0

local score = tonumber(redis.call('ZSCORE', KEYS[3], id))
if score then
  if ttl <= 0 or score >= now_s - ttl then
    redis.call('ZADD', KEYS[3], now_s, id)
    return {1, 0, 0}
  end
  -- The session went idle past its TTL; drop it and re-queue them below.
  redis.call('ZREM', KEYS[3], id)
end

local joined = 0
local rank = redis.call('ZRANK', KEYS[1], id)
if not rank then
  redis.call('ZADD', KEYS[1], redis.call('INCR', KEYS[5]), id)
  redis.call('HINCRBY', KEYS[6], 'joined', 1)
  rank = redis.call('ZRANK', KEYS[1], id)
  joined = 1
end
redis.call('ZADD', KEYS[2], now_s, id)
return {0, rank + 1, joined}
`)

// positionScript returns a waiting visitor's 1-based place in line, or 0 when
// they are not waiting at all.
//
//	KEYS: waiting
//	ARGV: id
var positionScript = redis.NewScript(`
local rank = redis.call('ZRANK', KEYS[1], ARGV[1])
if not rank then return 0 end
return rank + 1
`)

// heartbeatScript records that a waiting visitor is still watching.
//
//	KEYS: seen
//	ARGV: id, now_seconds
var heartbeatScript = redis.NewScript(
	`return redis.call('ZADD', KEYS[1], 'XX', ARGV[2], ARGV[1])`,
)

// flushScript empties the waiting queue, leaving admitted sessions alone so
// visitors already on the site are not thrown off it.
//
//	KEYS: waiting, seen
var flushScript = redis.NewScript(`
local n = redis.call('ZCARD', KEYS[1])
redis.call('DEL', KEYS[1], KEYS[2])
return n
`)

// hsetScript writes field/value pairs from ARGV into one hash.
//
//	KEYS: hash
//	ARGV: field, value, field, value, ...
var hsetScript = redis.NewScript(`
for i = 1, #ARGV, 2 do redis.call('HSET', KEYS[1], ARGV[i], ARGV[i + 1]) end
return 1
`)

// seedScript is hsetScript that never overwrites an existing value, so an
// operator's live tuning survives a restart.
var seedScript = redis.NewScript(`
for i = 1, #ARGV, 2 do redis.call('HSETNX', KEYS[1], ARGV[i], ARGV[i + 1]) end
return 1
`)

// anchorBucketScript starts the token bucket empty at a known instant. Without
// an anchor the first admission pass would have no elapsed time to bill for
// and would admit nobody; without HSETNX a restart would reset a bucket that
// another replica is already filling.
//
//	KEYS: bucket
//	ARGV: now_millis
var anchorBucketScript = redis.NewScript(`
redis.call('HSETNX', KEYS[1], 'tokens', '0')
redis.call('HSETNX', KEYS[1], 'last', ARGV[1])
return 1
`)

// touchScript refreshes an admitted session, returning 0 when the session has
// gone stale so the caller re-queues the visitor. This runs on every proxied
// request, so it is deliberately one round trip.
//
//	KEYS: active, conf
//	ARGV: id, now_millis
var touchScript = redis.NewScript(`
local now_s = tonumber(ARGV[2]) / 1000
local ttl   = tonumber(redis.call('HGET', KEYS[2], 'ttl_secs')) or 0
local score = tonumber(redis.call('ZSCORE', KEYS[1], ARGV[1]))

if not score then return 0 end
if ttl > 0 and score < now_s - ttl then
  redis.call('ZREM', KEYS[1], ARGV[1])
  return 0
end
redis.call('ZADD', KEYS[1], now_s, ARGV[1])
return 1
`)

// snapshotScript reads every counter a room reports in one consistent step.
// Active is counted with the TTL cutoff applied so the number matches what
// admission will actually see, rather than including sessions that are stale
// but not yet reaped.
//
//	KEYS: waiting, active, conf, stats
//	ARGV: now_seconds
//	Returns: {waiting, active, rate, cap, ttl, abandon, paused, joined, admitted, expired, abandoned}
var snapshotScript = redis.NewScript(luaPrelude + `
local now_s = tonumber(ARGV[1])
local conf  = readConf(KEYS[3])

local active
if conf.ttl > 0 then
  active = redis.call('ZCOUNT', KEYS[2], now_s - conf.ttl, '+inf')
else
  active = redis.call('ZCARD', KEYS[2])
end

local function stat(field)
  return tonumber(redis.call('HGET', KEYS[4], field)) or 0
end

-- Numbers cross the Lua boundary as strings to survive float rates intact.
return {
  tostring(redis.call('ZCARD', KEYS[1])),
  tostring(active),
  tostring(conf.rate),
  tostring(conf.cap),
  tostring(conf.ttl),
  tostring(conf.abandon),
  conf.paused and '1' or '0',
  tostring(stat('joined')),
  tostring(stat('admitted')),
  tostring(stat('expired')),
  tostring(stat('abandoned')),
}
`)
