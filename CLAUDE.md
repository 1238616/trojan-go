# CLAUDE.md

Guidance for Claude Code when working in this repository.

## Project

trojan-go — a Go implementation of the Trojan proxy protocol (module `github.com/p4gefau1t/trojan-go`, Go 1.25 per `go.mod`).

## Commands

- **Build:** `make normal` — release-style build into `./build/` with `-tags "full"`, `CGO_ENABLED=0`, version/commit stamped via ldflags. Quick check: `go build -tags "full" ./...`
- **Test:** `make test` — runs `SHADOWSOCKS_SF_CAPACITY="-1" go test -v ./...` (Bloom filter disabled during tests; keep this env var when running `go test` directly)
- **Lint:** `golangci-lint run` and `go vet ./...` (CI: `.github/workflows/linter.yml`)
- **Format:** `gofmt -w .`
- **Cross-compile:** `make linux-amd64`, `make darwin-arm64`, `make windows-amd64`, etc.; `make release` builds all platforms (fetches geoip/geosite `.dat` files via wget)

## Repo layout

- `main.go`, `easy/`, `option/`, `constant/`, `version/` — entry point, CLI flags, build constants
- `tunnel/` — transports and tunnel layers: `tls`, `websocket`, `mux`, `muxcool`, `singmux`, `socks`, `shadowsocks`, `simplesocks`, `trojan`, `tproxy`, `dokodemo`, `freedom`, `http`, `router`, `adapter`, `transport`
- `proxy/` — proxy client/server, forward, nat, custom modes, buffer/profiler
- `cluster/`, `statistic/`, `api/`, `redirector/`, `url/`, `log/`, `config/`, `common/` — feature modules
- `component/` — module registration (import `_ "…/component"` side-effect registration pattern)
- `example/` — sample JSON/YAML configs and systemd units; `test/` — test helpers; `docs/` — Hugo documentation site

## Notes

- The `full` build tag is required for complete binaries (`-tags "full"`).
- CI gates: `make test` (`.github/workflows/test.yml`) and golangci-lint (`.github/workflows/linter.yml`).
- `make install` / `make uninstall` touch system directories (`/usr/bin`, `/etc`, systemd) — never run without an explicit user request.
- Root-level `trojan-go` binary and `trojan-go-linux-amd64.zip` are build artifacts — don't commit new ones.
- Design/planning docs: `README.md`, `claude_agent.md`, `claude_design.md`, `deployment.md`, `update_plan.md`.
- ECC is installed globally via the plugin marketplace; use `ecc:golang-patterns`, `ecc:golang-testing`, `ecc:tdd-workflow`, `ecc:verification-loop` skills and the `ecc:go-reviewer` / `ecc:go-build-resolver` agents for Go work.
