# Atomic Site

Agent-first website builder. Single Go binary + SQLite + Astro. Ships a static `dist/` you own. Auto A+ security headers, GDPR-ready cookie banner embedded, agent API for AI-driven content edits.

You self-host it for one root domain at a time (`example.com` runs Atomic Site for `example.com`'s marketing site). Every customer-built site is just static files: serve from anywhere.

## Why

Most CMSes assume the cloud is the product. Atomic Site assumes the dist directory is. Run the binary on a 5 EUR VPS, ship the output to Vercel, Netlify, R2, S3, or any nginx behind your firewall. The agent API lets your AI of choice (Claude, OpenAI, local llama, anything that speaks HTTP) author content, blocks, settings, and pages without ever touching the dashboard.

## 60-second quickstart (local dev)

```bash
git clone <this-repo>
cd atomicsite

# 1. backend (port 8080)
make dev

# 2. frontend (separate terminal, port 5173 with proxy)
make frontend-install
make frontend-dev

# 3. open http://localhost:5173 and log in with the seeded admin
#    (default email: admin@atomicsite.dev, default password: changeme123)
```

Production builds embed the SvelteKit SPA into the Go binary:

```bash
make build           # frontend → cmd/server/frontend/build → bin/atomicsite
./bin/atomicsite     # serves admin + tracking + agent API on :8080
```

## Deployment matrix

| Target              | OSS Core | Notes                                                                      |
|---------------------|----------|----------------------------------------------------------------------------|
| Plain VPS + nginx   | yes      | The reference shape. `bin/atomicsite` + a per-site nginx vhost.            |
| Docker Compose      | yes      | See `docker-compose.example.yml`.                                          |
| Fly.io              | yes      | See `fly.toml`. Pre-issue TLS via Fly's edge.                              |
| Render / Railway    | yes      | Standard buildpack: `make build`, run `bin/atomicsite`.                    |
| Kubernetes          | yes      | Stateless app, single PVC for `/data` (SQLite + uploaded media + fonts).   |
| Dockyard            | yes      | Bright Interaction's own platform. Standard container deploy.              |
| Cloudflare Pages /  | yes      | For the **built sites** (the dist output), not the admin server itself.    |
| Vercel / Netlify    |          | Admin server runs anywhere; static output ships to the edge of choice.     |
| Atomic Site Cloud   | commercial | Hosted multi-tenant SaaS. Adds on-demand TLS, billing, SLA, support. Reach out if you'd rather not run infra. |

## Architecture

```
  ┌─────────────────────┐   ┌─────────────────┐
  │  Admin SPA          │   │  Agent API      │
  │  (SvelteKit, Bun)   │   │  (HTTP + MCP)   │
  └──────────┬──────────┘   └────────┬────────┘
             │                       │
             └───────────┬───────────┘
                         │
            ┌────────────▼────────────┐
            │  atomicsite (Go binary) │
            │  ┌───────────────────┐  │
            │  │ chi router        │  │
            │  │ sqlc / SQLite     │  │
            │  │ builder (Astro)   │  │
            │  │ analytics tail    │  │
            │  │ CookieProof embed │  │
            │  │ DuckDB rollups    │  │
            │  └───────────────────┘  │
            └────┬───────────────┬────┘
                 │               │
                 ▼               ▼
           ┌──────────┐     ┌─────────────┐
           │ /data    │     │ dist/       │
           │ SQLite + │     │ static site │
           │ media +  │     │ → any host  │
           │ fonts    │     └─────────────┘
           └──────────┘
```

## Configuration

Copy `.env.example` to `.env` and fill in values. The minimum for a working production deploy:

| Var                            | Why                                                         |
|--------------------------------|-------------------------------------------------------------|
| `ATOMICSITE_ENV`               | `production` (the default when unset) or `development`.     |
| `BASE_URL`                     | Public URL of the admin server (must be HTTPS in prod).     |
| `JWT_SECRET`                   | Session signing. `openssl rand -hex 32`.                    |
| `ANALYTICS_SALT`               | Visitor fingerprint salt. `openssl rand -hex 32`.           |
| `ATOMICSITE_SHIELD_KEY`        | 32-byte at-rest + MCP shield key. `head -c 24 /dev/urandom \| base64`. |
| `ATOMICSITE_PRIMARY_DOMAIN`    | The apex domain this instance serves (`example.com`).       |
| `ADMIN_PASSWORD`               | Initial admin login. Min 12 chars. Change before exposing.  |

The server refuses to start if any of these are unset or still equal their documented defaults.

**An absent `ATOMICSITE_ENV` means production.** That direction is deliberate. The posture used to be inferred from `BASE_URL` starting with `http://localhost`, and `BASE_URL` itself defaults to `http://localhost:8080`, so a single missing line in a deploy env file disabled every guard above at once while the container reported healthy. An env file can hand over a value; it cannot refuse an absence.

Local development opts out explicitly, and `make dev` already does it for you:

```bash
make dev                                  # sets ATOMICSITE_ENV=development
ATOMICSITE_ENV=development go run ./cmd/server   # the same thing by hand
```

Full env-var reference: see `.env.example`.

## Open Core boundary

Atomic Site ships as **Open Core**:

- **OSS Core** (this repo, Apache 2.0): everything you need to self-host one Atomic Site instance for one root domain. Go server, SQLite, builder, agent API, CookieProof embed, evaluation engine, retention manager, DuckDB analytics rollups, all the security defaults.
- **Cloud** (commercial, hosted by Bright Interaction or self-built behind the `ee` build tag): multi-tenant edge with on-demand TLS, billing, cross-tenant aggregation, cert pre-issuance queue, SLA-grade backups and DR.

Cloud features live behind a Go build tag (`ee/` directory, `//go:build ee`). The OSS distribution compiles with the no-op stubs; setting `-tags ee` is what links the cloud control plane in. The proprietary part of Atomic Site Cloud is the operation, not the code: the code can be inspected and the boundary is documented in `ee/README.md`.

If you self-host the OSS Core and outgrow it, exporting your data and migrating to the Cloud is a `pg_dump`-style operation. There's no lock-in either way.

## Building from source

Requirements:
- Go 1.26+ (CGO required: the analytics rollups use DuckDB)
- Bun 1.3+ (frontend, no exceptions)
- `sqlc` (only if you change SQL)
- A C toolchain (`build-essential` on Debian/Ubuntu, `gcc + g++ + libstdc++` on Alpine)

```bash
make frontend         # builds the SvelteKit admin SPA into cmd/server/frontend/build
make build            # builds the Go binary (embeds the frontend)
go test ./...         # runs the full Go test suite
go vet ./...          # static analysis
```

The Cloud build:

```bash
go build -tags ee -o bin/atomicsite-cloud ./cmd/server
```

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md). TL;DR: conventional commits, table-driven tests, no new env var without a `.env.example` entry, cloud-only code goes in `ee/`.

## Security

Atomic Site ships with security-first defaults: A+ headers, locked-down CSP, HttpOnly + Secure + SameSite=Strict session cookies, refuse-to-start on weak secrets, salted visitor fingerprints rather than IP storage. Report vulnerabilities to security@brightinteraction.com (please do not file public issues for security bugs).

## License

Apache License 2.0. See [LICENSE](LICENSE).
