// Command demo-origin is a stand-in for a real website, used by the Docker
// Compose stack so that getting admitted through anteroom is visible.
package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"sync/atomic"
	"time"
)

var hits atomic.Int64

func main() {
	addr := flag.String("listen", ":9000", "address to listen on")
	flag.Parse()

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		n := hits.Add(1)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		fmt.Fprintf(w, page, n, r.URL.Path, time.Now().Format(time.RFC1123))
	})

	log.Printf("demo origin listening on %s", *addr)
	srv := &http.Server{Addr: *addr, ReadHeaderTimeout: 10 * time.Second}
	log.Fatal(srv.ListenAndServe())
}

const page = `<!doctype html>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>You made it through</title>
<style>
  :root { color-scheme: light dark; }
  body { font: 16px/1.6 system-ui, sans-serif; max-width: 34rem; margin: 12vh auto; padding: 0 1.5rem; }
  h1 { font-size: 1.6rem; margin-bottom: .25rem; }
  dl { display: grid; grid-template-columns: auto 1fr; gap: .35rem 1rem; margin-top: 2rem; }
  dt { opacity: .6; }
  code { font-family: ui-monospace, monospace; }
</style>
<h1>You made it through.</h1>
<p>This is the origin site sitting behind anteroom. Reload as much as you
like — you keep your seat until your session goes idle.</p>
<dl>
  <dt>Visit number</dt><dd>%d</dd>
  <dt>Path</dt><dd><code>%s</code></dd>
  <dt>Served at</dt><dd>%s</dd>
</dl>
`
