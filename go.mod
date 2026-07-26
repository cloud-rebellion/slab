module github.com/bright-interaction/slab

go 1.26.3

// Pinned to the toolchain the container image already builds with. Without this,
// go.mod permits an older Go and govulncheck reports the standard-library
// vulnerabilities fixed in 1.26.5 (crypto/tls Encrypted Client Hello leak,
// net/textproto, crypto/x509, net/http/httputil) as reachable. The pin makes the
// version that keeps this safe a guarantee rather than a property of the base image.
toolchain go1.26.5

require (
	github.com/HugoSmits86/nativewebp v1.2.1
	github.com/alicebob/miniredis/v2 v2.38.0
	github.com/buckket/go-blurhash v1.1.0
	github.com/chromedp/chromedp v0.15.1
	github.com/getsentry/sentry-go v0.47.0
	github.com/go-chi/chi/v5 v5.3.0
	github.com/golang-jwt/jwt/v5 v5.3.1
	github.com/marcboeker/go-duckdb/v2 v2.4.3
	github.com/microcosm-cc/bluemonday v1.0.27
	github.com/redis/go-redis/v9 v9.18.0
	golang.org/x/crypto v0.53.0
	golang.org/x/image v0.43.0
	golang.org/x/net v0.56.0
	golang.org/x/sync v0.22.0
	modernc.org/sqlite v1.46.1
)

require (
	github.com/apache/arrow-go/v18 v18.4.1 // indirect
	github.com/aymerick/douceur v0.2.0 // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/chromedp/cdproto v0.0.0-20260321001828-e3e3800016bc // indirect
	github.com/chromedp/sysutil v1.1.0 // indirect
	github.com/dgryski/go-rendezvous v0.0.0-20200823014737-9f7001d12a5f // indirect
	github.com/duckdb/duckdb-go-bindings v0.1.21 // indirect
	github.com/duckdb/duckdb-go-bindings/darwin-amd64 v0.1.21 // indirect
	github.com/duckdb/duckdb-go-bindings/darwin-arm64 v0.1.21 // indirect
	github.com/duckdb/duckdb-go-bindings/linux-amd64 v0.1.21 // indirect
	github.com/duckdb/duckdb-go-bindings/linux-arm64 v0.1.21 // indirect
	github.com/duckdb/duckdb-go-bindings/windows-amd64 v0.1.21 // indirect
	github.com/dustin/go-humanize v1.0.1 // indirect
	github.com/go-json-experiment/json v0.0.0-20260214004413-d219187c3433 // indirect
	github.com/go-viper/mapstructure/v2 v2.4.0 // indirect
	github.com/gobwas/httphead v0.1.0 // indirect
	github.com/gobwas/pool v0.2.1 // indirect
	github.com/gobwas/ws v1.4.0 // indirect
	github.com/goccy/go-json v0.10.5 // indirect
	github.com/google/flatbuffers v25.2.10+incompatible // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/gorilla/css v1.0.1 // indirect
	github.com/klauspost/compress v1.18.0 // indirect
	github.com/klauspost/cpuid/v2 v2.3.0 // indirect
	github.com/marcboeker/go-duckdb/arrowmapping v0.0.21 // indirect
	github.com/marcboeker/go-duckdb/mapping v0.0.21 // indirect
	github.com/mattn/go-isatty v0.0.20 // indirect
	github.com/ncruces/go-strftime v1.0.0 // indirect
	github.com/pierrec/lz4/v4 v4.1.22 // indirect
	github.com/remyoudompheng/bigfft v0.0.0-20230129092748-24d4a6f8daec // indirect
	github.com/yuin/gopher-lua v1.1.1 // indirect
	github.com/zeebo/xxh3 v1.0.2 // indirect
	go.uber.org/atomic v1.11.0 // indirect
	golang.org/x/exp v0.0.0-20251023183803-a4bb9ffd2546 // indirect
	golang.org/x/mod v0.37.0 // indirect
	golang.org/x/sys v0.46.0 // indirect
	golang.org/x/telemetry v0.0.0-20260625142307-59b4966ccb57 // indirect
	golang.org/x/text v0.40.0 // indirect
	golang.org/x/tools v0.47.0 // indirect
	golang.org/x/xerrors v0.0.0-20240903120638-7835f813f4da // indirect
	modernc.org/libc v1.67.6 // indirect
	modernc.org/mathutil v1.7.1 // indirect
	modernc.org/memory v1.11.0 // indirect
)
