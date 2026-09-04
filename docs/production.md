# Running anteroom in production

Anteroom is a reverse proxy: traffic reaches it first, and it decides who
reaches your site. That means it has to survive the spike you are protecting
against, and it becomes a thing that can take your site down if you run it
badly. This is what to get right.

## Where it sits

```
DNS / CDN ──▶ your load balancer (TLS) ──▶ anteroom ──▶ your origin
```

Anteroom does not terminate TLS. Put it behind whatever already does, and set
`secure_cookies: true` so the visitor cookie is only sent over HTTPS.

This matters twice over for `/__anteroom/admin/`. The admin token is sent as an
`Authorization` header on every request the dashboard and every scrape make, so
anything that can read the connection can take the token and then reconfigure
or empty your queues. Reach the control room over HTTPS only, and if the
balancer can restrict that path to your own network, do that too.

If you cannot put a proxy in front of your site (managed platforms like
Vercel, Netlify and Cloudflare Pages own the edge) then anteroom in this form
does not fit. You would need to move ingress somewhere you control.

### nginx

```nginx
upstream anteroom { server 127.0.0.1:8080; }

server {
    listen 443 ssl http2;
    server_name shop.example.com;

    location / {
        proxy_pass http://anteroom;
        proxy_set_header Host              $host;
        proxy_set_header X-Forwarded-For   $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;

        # The waiting page holds a connection open to stream positions.
        # Buffering it would defeat the point, and a short read timeout would
        # cut visitors off mid-wait.
        proxy_buffering off;
        proxy_read_timeout 1h;
    }
}
```

### AWS ALB

Point a target group at anteroom's port with a health check on
`/__anteroom/healthz`. Raise the idle timeout well above the default 60
seconds, because position streams are long-lived connections and every drop
turns into a page reload from a visitor who is already waiting.

### Telling anteroom what is in front of it

```yaml
trusted_proxies:
  - 10.0.0.0/8        # your VPC or load balancer
  - ::1/128           # if anything connects over IPv6 loopback
```

**This is not optional if you use per-address join limits.** `X-Forwarded-For`
is believed only from these networks, because anyone can set that header. Get
it wrong and every visitor resolves to your load balancer's address, they all
share one rate-limit budget, and the limit throttles your entire site at once.

Anteroom logs a warning when it sees forwarded headers from a peer it was not
told to trust. Do not ignore it. Include IPv6 loopback if your balancer or
health checks connect that way, and it is the easiest one to miss.

## Redis

The queue lives in Redis, so Redis availability is anteroom's availability.

**Turn persistence on.** The demo Compose file disables it deliberately
(`--save "" --appendonly no`) because a demo has nothing worth keeping. In
production, enable AOF. Without it, a Redis restart loses every position and
everyone who has been waiting twenty minutes goes to the back of the line.

**Run it highly available:** managed Redis, Sentinel, or Cluster. Anteroom
reconnects on its own and needs no restart when Redis comes back, but while it
is gone nobody is admitted.

Memory is modest: a waiting visitor is a sorted-set member and a heartbeat
score. A queue of a million measured at 229 MiB of Redis, about 240 bytes a
visitor, so size for hundreds of megabytes rather than gigabytes. Anteroom
itself sat at 62 MiB with that million queued, because it holds no queue of
its own.

## When Redis is unreachable

By default, **nobody is admitted**. Visitors are held on the waiting page with
a 503 and let in when Redis returns. This is deliberate: waving everyone
through at the moment your queue store fails hands your origin the exact spike
anteroom exists to prevent.

If you would rather serve your site unprotected than serve nobody:

```yaml
fail_open: true
fail_open_after: 30s
```

Anteroom then proxies visitors straight through, but only after the queue has
been unreachable continuously for the grace period, so a one-second blip
releases nobody. While it is happening it logs at `ERROR`, emits a
`failing_open` event, and the dashboard shows an unmissable banner.
`GET /__anteroom/admin/api/status` reports this without touching Redis, so it
answers during exactly the incident it describes.

## Scaling

Run as many replicas as you like against one Redis. Admission is a single Lua
script and the rate budget is shared, so replicas cannot double-admit or
jointly exceed the configured rate. Three replicas against one Redis, at a
configured 1,000 admissions a second, were measured admitting 1,012 a second
in total rather than three times that.

The real constraint is **open connections, not CPU**. Every waiting visitor
holds a position stream, so 50,000 people waiting is 50,000 sockets, and a
held stream measured at about 28 KB of anteroom. Run out of descriptors and
anteroom logs `too many open files` and keeps serving the connections it
already has, so the symptom is arrivals failing rather than a crash. Raise
the file-descriptor limit accordingly:

```
LimitNOFILE=200000        # systemd
ulimit -n 200000          # shell
```

Anteroom itself is cheap per visitor: one Redis round trip per request, and
one Redis read per waiting visitor every two seconds regardless of how often
they refresh.

## Choosing the numbers

`rate` and `max_active` are the two that matter, and both are adjustable while
running, so start conservative and raise them while watching.

- **`max_active`** is the real ceiling on concurrent load. Set it to what your
  origin handles comfortably, not its breaking point.
- **`rate`** controls how fast the queue drains. Too high and you refill
  `max_active` faster than sessions end; too low and people wait needlessly.
- **`session_ttl`** decides how long an idle visitor keeps a slot. Shorter
  reclaims capacity faster but risks evicting someone mid-form.
- **`join_limit_per_ip`** defaults to 120 per minute. Watch `total_refused` on
  the dashboard: if it climbs during ordinary traffic, real visitors behind an
  office or mobile network are being turned away and it needs raising.

## Cutting over

1. Run anteroom pointed at your origin and check it with a `Host` header
   before any real traffic sees it.
2. Move the load balancer's target from your origin to anteroom.
3. Watch `total_admitted` and your origin's own metrics together.

To take anteroom out, point the balancer back at your origin. Nothing about
your site depends on anteroom, so removing it needs no code change.

## What to watch

| Signal | Meaning |
| --- | --- |
| `waiting` climbing steadily | Arrivals exceed `rate`; raise it if the origin is coping |
| `active` pinned at `max_active` | The cap is the bottleneck, not the rate |
| `total_refused` climbing | The per-address limit is turning real visitors away |
| `total_abandoned` high | People are giving up; the wait is too long |
| `failing_open` in the logs | **Your origin is unprotected right now** |

`GET /__anteroom/healthz` needs no token and is the endpoint to point a
load-balancer health check at.

### Scraping the numbers

Everything in that table is a series on `/__anteroom/admin/api/metrics`, in
Prometheus exposition format and labelled by room:

```yaml
scrape_configs:
  - job_name: anteroom
    metrics_path: /__anteroom/admin/api/metrics
    scheme: https
    authorization:
      credentials: <admin_token>     # or credentials_file, to keep it out of here
    static_configs:
      - targets: ["shop.example.com"]
```

`anteroom_waiting` and `anteroom_active` are gauges; the `_total` series are
counters, so graph them with `rate()`. `anteroom_failing_open` is the one to
alert on: it means anteroom is passing everyone through unchecked.

Scrape every anteroom replica rather than one behind the balancer: the totals
are shared in Redis and read the same everywhere, but health is per-process, so
scraping one replica hides an unhealthy sibling.

The control room also exports what one browser has seen: **Export CSV** on a
room downloads its queue depth, sessions, and admissions since that tab was
opened, which is the quick way to attach a picture of the queue to a
post-mortem without standing up Prometheus first.
