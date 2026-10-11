<h1>
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset=".github/assets/logo-dark.svg">
    <img src=".github/assets/logo-light.svg" alt="composewright" height="56">
  </picture>
</h1>

**Paste a GitHub link. Get a `compose.yaml` and a documented `.env` that actually start.**

composewright reads a repository (or a README, or a compose file you already have), works out how the app is meant to be deployed, writes a Compose file plus a `.env` with every variable explained and every secret generated, and then **proves the stack boots** by running it in an isolated, resource-capped sandbox before handing it to you. Runs as a normal user, works with a local Ollama, and never phones home.

> Status: pre-alpha. Nothing below is released yet — this README describes the intended first release so the install path is designed before the code.

## Quickstart — one image, one command

```sh
docker run -d --name composewright --restart unless-stopped \
  -p 127.0.0.1:8484:8484 \
  -v composewright-data:/data \
  -v /var/run/docker.sock:/var/run/docker.sock \
  --group-add "$(stat -c '%g' /var/run/docker.sock)" \
  ghcr.io/aiulian25/composewright:latest
```

Open **http://127.0.0.1:8484**, set an admin password, and choose a model:

- **Local (recommended):** Ollama at `http://127.0.0.1:11434` with a coding model such as `qwen2.5-coder:14b`. Nothing leaves your machine.
- **Cloud:** paste an Anthropic or OpenAI-compatible API key. The key is stored encrypted; README content is sent to that provider, your secrets never are.

That is the whole install. The image contains everything it needs: the app, the web UI, its database, and the `docker compose` tooling it uses to validate stacks. No sidecars, no second container.

### Prefer a compose file?

`deploy/composewright.yaml` is the same as one service:

```sh
DOCKER_GID=$(stat -c '%g' /var/run/docker.sock) docker compose -f deploy/composewright.yaml up -d
```

### Running on rootless Docker

Run it on your rootless daemon as uid 0 — inside a rootless daemon that *is* your own unprivileged user — with that daemon's socket and a data folder you own. A volume would belong to the image's uid 65532, and composewright refuses a data directory owned by another user than the one it runs as:

```sh
mkdir -p -m 0700 ~/.local/share/composewright
docker run -d --name composewright --restart unless-stopped \
  -p 127.0.0.1:8484:8484 \
  -v ~/.local/share/composewright:/data \
  -v "$XDG_RUNTIME_DIR/docker.sock:/var/run/docker.sock" \
  --user 0:0 \
  ghcr.io/aiulian25/composewright:latest
```

`composewright doctor --setup-rootless` prints the steps for the host.

### Synology (DSM 7.2+)

Container Manager → Project → paste `deploy/composewright.yaml` and set `DOCKER_GID` to the gid of `/var/run/docker.sock` (`stat -c '%g' /var/run/docker.sock` over SSH).

## Using it

1. **Source** — drop files (README, compose, `.env`, Dockerfile), paste a repository URL, or paste a compose file and `.env` you already have.
2. **Analyse** — the repo is fetched as a tarball through the host's API (no `git clone`, no hooks); environment variables, images, ports and volumes are extracted with their file and line; rules turn that into a plan, and a model fills what they leave open only if you choose one.
3. **Review** — every variable shows where it came from; secrets are generated for you; anything that needs host networking, devices or privileged mode is a request *you* grant, never a default. You can edit compose.yaml yourself before the test; the policy gate checks your version too.
4. **Validate** — the stack runs in a throw-away Compose project with memory, CPU and pid limits, ports bound to loopback, and `restart: "no"` so crash loops surface. Health checks and logs decide pass or fail; with a model, failures are repaired and retried up to three times.
5. **Result** — copy the files, download them as a zip, save them to a folder named after the container (rename it if you like), or push them straight into Arcane's templates or projects folder. Delete or start over from the same screen.

Everything above also works from the CLI:

```sh
composewright gen https://github.com/miniflux/v2
composewright show miniflux
composewright export miniflux --to ./miniflux
```

## Ways to run it

| How | Start it with | Notes |
|---|---|---|
| `docker run` | the quickstart above | A named volume and the socket's group |
| Compose | `deploy/composewright.yaml` | The same as one service |
| Hardened compose | `deploy/composewright.hardened.yaml` | A socket proxy on an internal network, read-only containers, a data folder owned by uid 65532 |
| Rootless Docker | the command above | Your own daemon; data in a folder you own |
| Synology DSM 7.2+ | Container Manager → Project | `deploy/composewright.yaml` |

The container image is the only distribution. To work on composewright itself, see `CONTRIBUTING.md`.

## Security posture

- **Non-root everywhere.** The container runs as uid 65532 and needs only the Docker socket and its data volume; a rootless Docker daemon is the recommended sandbox and `doctor` tells you how to set one up.
- **The model never writes YAML.** It returns a restricted plan that cannot express `privileged`, host mounts or `build:`; composewright's own code renders the Compose file and enforces a policy gate independent of the model.
- **Nothing runs without you.** You approve the plan before validation; privilege requests are explicit checkboxes.
- **Secrets stay encrypted.** `.env` values are stored with authenticated encryption under a key that lives outside the database; they are written to disk only for the seconds a run needs them and only in saved exports you ask for.
- **No phone-home.** No telemetry, no update checks, no CDN assets, no web fonts. Outbound connections go only to the model endpoint you configured, the code host you asked for, and container registries for image checks — and a build-time check enforces that.

The Docker socket is root-equivalent on the host; the UI is therefore bound to `127.0.0.1` by default and always requires a login. An optional hardened compose file with a socket proxy is `deploy/composewright.hardened.yaml`; its comments explain the setup.

## What it is not

Not a PaaS, not a reverse proxy, not an app store, and it does not build images from source. When a stack passes, export it to whatever you already run — Arcane, Dockge, Portainer, Dokploy, or plain `docker compose up -d`.

## Development

Go 1.26+, `CGO_ENABLED=0`. `make lint test build`. See [CONTRIBUTING.md](CONTRIBUTING.md), and [SECURITY.md](SECURITY.md) for disclosure.

## License

MIT. See [LICENSE](LICENSE) and [THIRD_PARTY_LICENSES.md](THIRD_PARTY_LICENSES.md).
