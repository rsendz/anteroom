// Its own module on purpose: the load test needs a running anteroom and a real
// Redis, so it must stay out of `go test ./...` and `make check`.
module anteroom-loadtest

go 1.24
