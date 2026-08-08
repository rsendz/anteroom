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
`
