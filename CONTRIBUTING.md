# Contributing

Thank you for helping. composewright turns repositories into deployment files that people run on their own machines, so changes are held to a high bar for correctness and security. Small, focused pull requests are reviewed fastest.

## Set up

- Go 1.26 or later, `make`, GNU grep, and Docker Engine with the Compose plugin 2.24.4 or later for the sandbox tests. `go.mod` names the
  patched toolchain to build with (`toolchain go1.26.9`); with the default `GOTOOLCHAIN=auto`, an older `go` fetches it once, verified
  against the checksum database. A machine that sets `GOTOOLCHAIN=local` needs that release installed.
- `make tools` installs the pinned linters into `bin/tools`.

## Build and run

`make build` writes the static binary to `bin/composewright` (`CGO_ENABLED=0`, `-trimpath -buildvcs=false`). `make image` builds the container image for this machine (`IMAGE=` names it, `composewright:dev` by default), and `make dev` runs the development container on port 8484 from a fresh build. The image is the only way composewright is distributed; a version tag builds, signs and publishes it (`.github/workflows/release.yml`).

## Before every commit

```bash
make lint test build
```

`make lint` runs golangci-lint, the colour and no-CDN checks for browser assets (`scripts/lint-*.sh`) and the licence allowlist. `make docs` writes a CLI reference from the commands' help to `docs/cli.md`, which stays out of the repository.

Slower suites:

| Target | Needs | What it proves |
|---|---|---|
| `make test-docker` | Docker | Real sandbox runs: healthy, crash, unhealthy, timeout, fatal log line |
| `make egress` | Network | Every host contacted is on the allowlist, through a recording proxy |
| `make offline` | `unshare` | init, gen and show work with no network at all |
| `make eval` | Optional Docker, network and a model | Policy and validation pass rates per engine on the corpus |
| `make container-smoke` | Docker | The image starts the UI on an empty volume and runs init, doctor, gen, show and export as uid 65532 |

## Conventions

- **Commits** follow [Conventional Commits](https://www.conventionalcommits.org/) (`feat:`, `fix:`, `docs:`, `test:`, `refactor:`), one logical change each. Update `CHANGELOG.md` under Unreleased in the same commit.
- **No TODO without an issue reference**, e.g. `TODO(#42): ...`. A deliberate simplification is marked `ponytail:` with its limit and how to lift it.
- **Comments** explain what the code cannot say itself: a reason, a constraint, a surprising choice. They do not narrate the code.
- **Names** are full words; constants replace magic numbers and strings.
- **Boundaries are enforced by the linter:** only `internal/netguard` makes outbound connections, only `internal/sandbox` starts processes, and only `internal/store` touches SQL. Browser assets use the tokens in `web/static/tokens.css` and load nothing from the network.
- **Tests are part of the change:** table-driven tests for rules and policy, adversarial plans for anything the policy gate decides (`testdata/adversarial/`), golden files for rendered output, fuzz tests for parsers.

## Security-sensitive changes

Changes to the policy gate, the plan schema, netguard, the sandbox override, secret handling or redaction need a test that fails without the change. Do not report vulnerabilities in public issues; see [SECURITY.md](SECURITY.md).

## AI-assisted contributions

See [AI_POLICY.md](AI_POLICY.md).
