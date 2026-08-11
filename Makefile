.PHONY: dev frontend frontend-dev frontend-install build build-server vet sqlc clean

# Run Go server in dev mode (frontend served separately via `make frontend-dev`)
#
# ATOMICSITE_ENV=development is what opts out of the production boot guards
# (JWT_SECRET, ANALYTICS_SALT, the 32-byte shield key, HTTPS, ADMIN_PASSWORD
# strength). Absent, the server treats itself as production and refuses to
# start with the sentinel defaults, which is the point: a forgotten line in
# a deploy env file must fail closed, so the dev relaxation has to be an
# explicit request rather than something inferred from a localhost URL.
dev:
	ATOMICSITE_ENV=development go run ./cmd/server

# Build SvelteKit frontend and sync into the embed directory
frontend:
	cd frontend && bun run build
	rm -rf cmd/server/frontend/build
	mkdir -p cmd/server/frontend
	cp -r frontend/build cmd/server/frontend/build

frontend-install:
	cd frontend && bun install

frontend-dev:
	cd frontend && bun run dev

# Full production build: frontend then Go binary with embedded SPA
build: frontend build-server

build-server:
	go build -o bin/atomicsite ./cmd/server

vet:
	go vet ./...

sqlc:
	sqlc generate

clean:
	rm -rf bin/ cmd/server/frontend/build/
	cd frontend && rm -rf build/ .svelte-kit/ node_modules/
