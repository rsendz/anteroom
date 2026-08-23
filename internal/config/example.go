package config

// Example is the commented starting config written by `anteroom init`. The two
// verbs are the cookie secret and the admin token, which are generated fresh
// so that a new deployment never ships with a guessable one.
const Example = `# anteroom — a self-hosted virtual waiting room.
#
# Point your DNS or load balancer at anteroom instead of your site, set
# origin below to where your site actually runs, and you are done. Every
# setting here can also be given as an environment variable, e.g.
# ANTEROOM_LISTEN, ANTEROOM_REDIS_ADDR, ANTEROOM_KAFKA_BROKERS.

listen: ":8080"

# Keep this secret and stable: it signs the cookie that remembers a visitor's
# place in line. Changing it sends everyone to the back of the queue.
cookie_secret: "%s"

# Bearer token for the dashboard at /__anteroom/admin/ and its API.
admin_token: "%s"

# Set to true when visitors reach anteroom over HTTPS.
secure_cookies: false

# The networks anteroom sits behind. X-Forwarded-For is believed only on
# requests arriving from these, because anyone can set that header and
# per-address limits would otherwise mean nothing. Leave empty if visitors
# connect to anteroom directly.
trusted_proxies: []
#   - 10.0.0.0/8        # your VPC or load balancer
#   - 172.16.0.0/12

# How often admissions are considered. The default is fine.
admit_interval: 250ms

# If the queue store is unreachable, anteroom holds visitors on the waiting
# page and admits nobody. Turning this on lets them through instead, once the
# outage has lasted fail_open_after -- trading your origin's protection for
# the site staying up. It is off by default because a waiting room that
# quietly stops queueing is not a waiting room.
fail_open: false
fail_open_after: 30s

redis:
  addr: "localhost:6379"

# Optional. With no brokers, anteroom runs exactly the same and simply does
# not publish events.
kafka:
  brokers: []
  topic: "anteroom.events"

rooms:
  # One room per site you are protecting. The room name is used in Redis keys
  # and event payloads.
  main:
    # Which hostname this room answers for. Leave it out on one room to make
    # that room the catch-all for any host.
    # match_host: shop.example.com

    # Where admitted visitors are sent.
    origin: "http://localhost:3000"

    # Visitors let in per second. Set this to what your site can actually
    # absorb, then raise it from the dashboard once you see it holding.
    rate: 5

    # Visitors allowed on the site at once. Admission needs both a free slot
    # and rate budget, so this is the real ceiling on concurrent load.
    max_active: 500

    # A session with no requests for this long is reclaimed and its slot
    # given to the next person in line.
    session_ttl: 5m

    # A queued visitor whose page has gone quiet for this long is assumed to
    # have left, and stops holding up the people behind them.
    abandon_after: 60s

    # How many visitors may newly join the queue from one address in
    # join_limit_window. This is what stops a script taking thousands of
    # places. Set to 0 to disable.
    #
    # Keep it generous: office networks and mobile carriers put many real
    # people behind a single address. Watch total_refused on the dashboard —
    # if it climbs during normal traffic, the limit is too tight.
    join_limit_per_ip: 120
    join_limit_window: 1m

    # Shown on the waiting page.
    title: "Just a moment"
    message: ""

    # Forward the visitor's Host header to the origin instead of the origin's
    # own. Turn this on for a backend that serves several virtual hosts.
    preserve_host: false

    # Open the room at a fixed time, for a sale or a drop. Times are RFC 3339.
    # Before queue_opens_at nobody is queued at all; between it and admits_at
    # visitors are collected but nobody is let in; after closes_at there are no
    # new admissions, though visitors already on the site keep their sessions.
    # schedule:
    #   queue_opens_at: 2026-11-20T09:30:00Z
    #   admits_at:      2026-11-20T10:00:00Z
    #   closes_at:      2026-11-20T12:00:00Z

    # With a schedule, draw for places among everyone collected before the
    # doors open rather than ordering them by arrival, so turning up early
    # gains nothing. A visitor's place comes from who they are, so leaving and
    # rejoining does not reroll it.
    # lottery: true
`
