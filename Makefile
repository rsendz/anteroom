BINARY := bin/anteroom

.PHONY: help
help:
	@echo "anteroom"
	@echo
	@echo "  make build     build the binary (front-end included)"
	@echo "  make web       build the front-end only"
	@echo "  make test      run the Go tests with the race detector"
	@echo "  make check     test, vet, and type-check the front-end"
	@echo "  make demo      run the whole stack in Docker (visit localhost:8080)"
	@echo "  make demo-down stop the demo stack"
	@echo "  make clean     remove build output"

# The front-end is embedded in the binary, so it has to exist before `go build`.
.PHONY: build
build: web
	go build -o $(BINARY) ./cmd/anteroom

.PHONY: web
web: web/node_modules
	cd web && npm run build

# A real file target, so a rebuild does not reinstall from scratch every time.
web/node_modules: web/package.json web/package-lock.json
	cd web && npm ci --no-audit --fund=false
	touch $@

.PHONY: test
test:
	go test ./... -race

# Depends on node_modules so that `make check` works on a fresh clone.
.PHONY: check
check: test web/node_modules
	go vet ./...
	gofmt -l cmd internal | tee /dev/stderr | (! read)
	cd web && npm run check

.PHONY: demo
demo:
	docker compose -f deploy/docker-compose.yml up --build

.PHONY: demo-down
demo-down:
	docker compose -f deploy/docker-compose.yml down -v

.PHONY: clean
clean:
	rm -rf bin internal/httpserver/web/static
