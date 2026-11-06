# Contributing

Thank you for helping. composewright turns repositories into deployment files that people run on their own machines, so changes are held to a high bar for correctness and security. Small, focused pull requests are reviewed fastest.

## Set up

- Go 1.26 or later, `make`, GNU grep, and Docker Engine with the Compose plugin 2.24.4 or later for the sandbox tests. `go.mod` names the
  patched toolchain to build with (`toolchain go1.26.10`); with the default `GOTOOLCHAIN=auto`, an older `go` fetches it once, verified
  against the checksum database. A machine that sets `GOTOOLCHAIN=local` needs that release installed.
- `make tools` installs the pinned linters into `bin/tools`.

## Build and run

`make build` writes the static binary to `bin/composewright` (`CGO_ENABLED=0`, `-trimpath -buildvcs=false`). `make image` builds the container image for this machine (`IMAGE=` names it, `composewright:dev` by default), and `make dev` runs the development container on port 8484 from a fresh build. The image is the only way composewright is distributed; a maintainer manually dispatches `.github/workflows/release.yml` on an existing `vMAJOR.MINOR.PATCH` tag. Pushing a tag alone publishes nothing. The workflow builds a native amd64 OCI candidate with SBOM/provenance, tests and scans it, then copies that archive with digest preservation and signs the index. It never rebuilds after approval by the gates.

The bundled Compose helper is built from checksum-pinned upstream 5.7.0 source
with `golang.org/x/net` 0.62.0 for CVE-2026-78661 and the patched
`github.com/klauspost/compress` 1.20.2. It reports
`5.7.0-composewright.1` to distinguish the patched build; the exact image scans
must still pass. Docker CLI remains the digest-pinned upstream 29.9.2 binary.

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
| `make container-smoke` | Docker, Go, jq, privileged disposable test runner | Packaged ordinary-daemon startup, ownership, persisted intake/export, actual CLI validation, file/secret transfer, cancellation and cleanup |

`make container-smoke` builds this machine's image, then creates a dedicated Docker-in-Docker daemon. `scripts/container-smoke.sh IMAGE ordinary` tests an already built image; repeat with `rootless` and `proxy` for the other supported connections. This requires a disposable Linux VM permitting privileged Docker-in-Docker (including rootless user namespaces/cgroups). Do not run privileged test infrastructure on production hosts. The outer host socket is used only to create/remove the uniquely named test daemon and load the image into it; the candidate receives only the inner socket or the shipped proxy. No host data folders or production daemon socket enter the test daemon. No host-wide prune runs.

The native test binary uses the candidate's Docker/Compose executables in a separate client filesystem. All connections check exact dummy file/secret bytes (including literal dollar signs and a custom secret target), cancellation and cleanup. Ordinary/proxy runs also execute the full PostgreSQL/nginx, readiness/failure and resource-metrics suite. Isolated rootless Docker-in-Docker lacks delegated cgroups, so it makes no resource-enforcement or metrics claim. Sentinel containers, volumes and networks inside the disposable daemon must survive validation unchanged; owned sandbox resources must be absent. The entire daemon, its tmpfs data, and any anonymous volumes are removed by the exit trap even on failure. Logs and final inventories remain outside it in `COMPOSEWRIGHT_TEST_LOGS` (otherwise a printed `/tmp` directory).

CI runs both architectures and all three connections through `.github/workflows/candidate.yml`. `scripts/scan-candidate.sh OCI_ARCHIVE EVIDENCE_DIRECTORY` requires Trivy, skopeo and jq. It retains the full vulnerability report and CycloneDX SBOM; HIGH/CRITICAL findings, including unfixed findings, block publication. There are no active waivers: the old Compose-specific exceptions lack evidence for the current bundled version and were removed. Any future exception must identify the exact installed version/path, primary applicability evidence and expiry. Candidate archives (including build provenance) and all logs are retained as workflow artifacts for 14 days; release scan reports/SBOMs are also release assets. An unavailable topology or scanner is a failed gate, never a skipped pass.

A maintainer can dispatch `release.yml` on a temporary candidate branch with
`candidate_only=true` and `candidate_version=X.Y.Z`. The publishing job is skipped
and candidate jobs retain read-only repository permissions. Native amd64
runners produce the tested archives for local, digest-preserving publication.
Rootless user namespaces are enabled only on those disposable runners; local
host security settings are not changed. A failed or unavailable topology still
blocks a release. The publishing mode accepts no version override and still
requires an existing version tag.

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
