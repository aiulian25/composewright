# Changelog

All notable changes to this project are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and the project uses
[Semantic Versioning](https://semver.org/).

## [Unreleased]

### Added

- **Generation.** `composewright gen` turns a GitHub link (the latest
  release by default, or `--ref`) or a local folder into compose.yaml, a
  `.env` with every variable explained and every secret generated, and
  DEPLOY.md with what is left to do. The web UI also takes uploaded files or
  a pasted compose file, with or without your own `.env`, whose real secrets
  are kept.
- **Facts with provenance.** Compose files (found by content), Dockerfiles,
  `.env` samples, READMEs and docs (`docker run` commands, variable tables,
  image names, default credentials), code, Helm values and CI workflows are
  read with the file and line every value came from, and prompt-injection
  text is flagged. Archives are unpacked with size, count and expansion
  limits.
- **Rules first.** Deterministic rules choose the compose file a deployment
  uses, find the app service, replace builds with the published image, turn
  bind mounts into named volumes, generate the secrets the stack owns and
  point connection strings at them, add a missing MariaDB or Postgres when
  the settings ask for one, take healthchecks and memory needs of known
  images from a curated catalogue, and fall back to the README's
  `docker run`. Each value carries a confidence, and gaps are listed.
- **Images checked.** Every allowed image is resolved and pinned by digest,
  its own healthcheck, ports and volumes feed the plan, and its source label
  is checked against the repository. Missing images are refused.
- **An optional model.** Anthropic, OpenAI-compatible servers and Ollama can
  fill what the rules leave open (`assisted`) or draft the whole plan
  (`model`); the default engine is the rules alone. The model sees redacted
  facts as data and never a secret, answers in a JSON schema, and cannot
  change images, needs, services or confident rule values. After a failed
  test it may repair commands, healthchecks, ports, dependencies and
  non-secret variables.
- **Two policy gates.** The plan and the rendered compose.yaml, re-parsed
  with compose-go, are both checked, and so is a compose.yaml you edit:
  images come only from allowed registries, and host networking, devices,
  privileged mode, host bind mounts, low ports and added capabilities happen
  only when you grant them.
- **Sandbox validation.** Once you approve, the stack runs as a throw-away
  Compose project with no new privileges, resource caps, loopback-only
  ports, no external networks and throw-away volumes. A probe asks the app
  over the sandbox network the way a proxy would. The verdict says why a run
  failed, with redacted logs and the last healthcheck output, and the
  sandbox is always removed.
- **Your environment profile.** Expose an app through your reverse proxy or
  tunnel networks (Traefik, Nginx Proxy Manager, Caddy, Pangolin,
  cloudflared, Tailscale and others, each with an optional domain), on
  127.0.0.1, on the LAN or not at all; use named volumes or bind mounts
  under a base path; set the time zone, user IDs, restart policy, logging
  and resource limits.
- **The web UI** (`composewright serve`): Home, the five steps (Source,
  Analyse, Review, Validate, Result) with live progress, an editor for
  compose.yaml and `.env` that checks as you type and shows where every line
  came from, versions of each project, and Settings for the profile, the
  engine and model providers, appearance and the account. Dark and light
  themes; nothing loads from the internet.
- **Sign-in.** Argon2id password hashing, hashed sessions, CSRF tokens,
  Origin and Host checks, a strict Content-Security-Policy and a login
  throttle, which counts each visitor behind a reverse proxy once
  `serve --trusted-proxy` names the proxy. Plain HTTP on a home network
  works, the first account is created from the page within 15 minutes of a
  start, with a warning in serve's output naming the address it came from,
  and `composewright password` sets a new password.
- **Secrets and state.** Secret values and API keys are sealed with
  XChaCha20-Poly1305 in a SQLite database, `keys rotate` re-seals them under
  a new master key, actions are recorded in a hash-chained audit log, and
  nothing composewright writes is readable by other users.
- **Egress control.** composewright's own connections all go through
  `internal/netguard`: each purpose has a host allowlist, and internal
  addresses are refused after DNS resolution unless configured as private
  targets. A model is reached over plain HTTP only at a local address (a
  private or Tailscale IP, or a LAN name), so an API key never crosses the
  internet unencrypted. `gen --offline` keeps composewright itself off the
  network.
- **Commands:** `init`, `config`, `keys`, `gen`, `list`, `show`, `rm`,
  `export` (to a folder or Arcane templates), `profile`, `validate`,
  `doctor`, `serve`, `password` and `version`.
- **The container image**, the way to run composewright: distroless, uid
  65532, with the docker CLI and compose plugin, `/data` as the only volume
  and every base image pinned by digest. `deploy/` has a one-service compose
  file and a hardened variant behind a Docker socket proxy.
- **Development:** `make lint test build`, `make eval` on 20 pinned corpus
  entries, and `make test-docker`, `container-smoke`, `egress`, `offline`,
  `vuln`, `licenses` and `docs`.
- MIT licence, with CONTRIBUTING.md, SECURITY.md and AI_POLICY.md.
