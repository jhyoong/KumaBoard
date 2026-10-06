# AGENTS.md

KumaBoard: self-hosted control plane for a multi-OS homelab. One Go module
(`github.com/jhyoong/KumaBoard`), two binaries: `cmd/kumaboard` (server) and
`cmd/kuma-agent` (agent). React dashboard in `web/` embedded into the server.

## Commands

- Go tests: `make test` (= `go test ./...`). Single package/test:
  `go test ./server/hub` / `go test ./server/hub -run TestName`.
- Full build: `make all` (test + server + 4 cross-compiled agent targets).
- Server only: `make server` (builds web first, outputs `bin/kumaboard`).
- Web (from `web/`): `npm run build` (= `tsc -b && vite build`),
  `npm run lint` (oxlint, NOT eslint), `npm test` (vitest run).
- `make fmt` only lists unformatted files (`gofmt -l .`); run `gofmt -w` yourself.
  `make vet` runs `go vet ./...`.
- Deploy one device: `make deploy-<device>` (build + scp + remote restart).
  Devices are defined in `deploy/hosts.local.mk`, which is gitignored; copy
  `deploy/hosts.example.mk` to create it. Never put real SSH users or
  addresses in `deploy/hosts.mk` or any other tracked file.

## Build gotchas

- `web/embed.go` does `go:embed all:dist` and `web/dist` is not committed.
  A fresh clone must run `make web` (or `cd web && npm ci && npm run build`)
  before `go build ./cmd/kumaboard` succeeds. Use `make server` to get the order right.
- `CGO_ENABLED=0` everywhere (exported in the Makefile). SQLite is
  `modernc.org/sqlite` (pure Go). Never introduce a cgo dependency.
- Requires Go 1.26+.

## Architecture rules that bite

- `proto/` is the JSON-over-WSS wire protocol shared by both binaries and must
  not import `server/` or `agent/`. Protocol versioning follows an N/N-1
  window (`proto/version.go`); check `Supported()` semantics before changing
  `Version`/`MinSupported`.
- Server is TLS-only on 8443. It creates its own CA + server cert under
  `data_dir/pki/` on first start. There is no plain-HTTP mode.
- Two fully separate auth domains: dashboard `/api/*` uses the `kb_session`
  cookie (Argon2id passwords, server-side sessions); agent traffic uses
  `Authorization: Bearer <token>` + `X-Device-Name` headers. Do not cross-wire them.
- Every request passes Host/Origin allowlist checks derived from `listen_addrs`
  and `hostnames` (unknown Host => HTTP 421). Local runs need matching config.
- CLI defaults are absolute production paths (`-config` defaults to
  `/etc/kumaboard/config.yaml`; agent also honors `$KUMA_AGENT_CONFIG`). For
  local runs pass `-config` a writable path with a local `data_dir`.
- Agent platform-specific code is split into `*_unix.go` / `*_windows.go` files
  (`cmd/kuma-agent`, agent terminal/upgrade/env/collectors). When touching those
  areas, keep both halves compiling: verify with
  `GOOS=windows GOARCH=amd64 go build ./...`.

## Testing

- `internal/integration/` boots a real server and real agents in one process
  over real TLS with a temp SQLite DB (`harness_test.go`). No external
  services needed for any test. Prefer adding an integration test there for
  end-to-end behavior instead of mocking.
- No snapshot or fixture workflow; plain `go test` / `vitest run`.

## Release signing / upgrade flow

- Release verification keys are compiled into agents via ldflags
  (`RELEASE_KEY_CURRENT` / `RELEASE_KEY_NEXT` injecting into `internal/buildinfo`).
  Dev keypair: `configs/dev-release-keys.txt` (public, committed) and
  `configs/dev-release.key` (private, gitignored — never commit it).
- Full flow: `make release VERSION=x.y.z RELEASE_KEY_CURRENT=... ` ->
  `kumaboard sign --version x.y.z` (reads `data_dir/pki/release_ed25519`,
  root-only on the control plane) -> `kumaboard release ingest` -> set target
  version per device in dashboard. Agent self-verifies, swaps binary, and
  auto-rolls back if it cannot handshake within 120s.
- `make release` refuses `VERSION=dev`; always pass an explicit version.

## Docs

- Plan docs are local-only (gitignored, not in the repo). When present,
  `docs/PLAN.md` is the design source of truth; `docs/PHASES.md` indexes the
  ordered phase files. Phases run strictly in number order. README's
  "current status" lags the code — check `docs/` and `git log`, not README,
  for what is implemented.
