<h1>
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset=".github/assets/logo-dark.svg">
    <img src=".github/assets/logo-light.svg" alt="composewright" height="56">
  </picture>
</h1>

**Paste a GitHub link. Get a `compose.yaml` and a documented `.env` that actually start.**

composewright reads a repository (or a README, or a compose file you already have), works out how the app is meant to be deployed, writes a Compose file plus a `.env` with every variable explained and every secret generated, and then **proves the stack boots** by running it in an isolated, resource-capped sandbox before handing it to you. Runs as a normal user, works with a local Ollama, and never phones home.

![The start page: paste a GitHub link, drop files, or paste a compose file and .env](.github/assets/screenshots/home.png)

> Status: 0.1.1. Expect rough edges and breaking changes before 1.0; the [CHANGELOG](CHANGELOG.md) lists what each release changes.

**On this page:** [Install](#install) · [First login](#first-login) · [Your first project](#your-first-project) · [Environment variables](#environment-variables) · [Settings](#settings) · [Reach it from other machines](#reach-it-from-other-machines) · [Update, back up, recover](#update-back-up-recover) · [Troubleshooting](#troubleshooting)

## Install

You need a Linux machine (amd64 or arm64) with Docker Engine and its compose plugin. composewright is one container: there is no other service to install and no `.env` to write.

### 1. Save the compose file

Make a folder for it, for example `~/composewright`, and save this as `compose.yaml` in it. It is also [`deploy/composewright.yaml`](deploy/composewright.yaml).

```yaml
services:
  composewright:
    image: ghcr.io/aiulian25/composewright:0.1.1    # add @sha256:… from the release notes to pin these exact bytes
    container_name: composewright
    restart: unless-stopped
    ports:
      - "127.0.0.1:8484:8484"                     # this machine only; put a reverse proxy in front for LAN or TLS
    volumes:
      - composewright-data:/data                    # config, database, master key and projects; a host folder works too
      - /var/run/docker.sock:/var/run/docker.sock   # required: composewright tests stacks with your Docker
    read_only: true                                 # it writes only to /data and the /tmp below
    tmpfs: [/tmp]
    cap_drop: [ALL]
    cap_add: [CHOWN, SETGID, SETUID]                # used once at start to make /data its own and join the socket's group, then it runs as uid 65532 with none
    security_opt: ["no-new-privileges:true"]
volumes:
  composewright-data: {}
```

What each part is for:

| Line | Why |
|---|---|
| `127.0.0.1:8484:8484` | The web UI, on this machine only. To open it from elsewhere, see [Reach it from other machines](#reach-it-from-other-machines). |
| `composewright-data:/data` | Settings, database, master key, projects and backups. A host folder works too, for example `- /srv/composewright:/data`. You don't need to create it or set permissions: on the first start composewright takes over a new or empty folder (owner uid 65532, mode 700). |
| `/var/run/docker.sock` | **Required.** composewright tests every stack with your Docker. On rootless Docker, put your daemon's socket on the left: `$XDG_RUNTIME_DIR/docker.sock`. |
| `read_only`, `tmpfs`, `cap_drop`, `security_opt` | Hardening: a read-only container with no capabilities. |
| `cap_add: [CHOWN, SETGID, SETUID]` | Used once at start to make `/data` its own and join the Docker socket's group; after that it runs as uid 65532 with none. Keep it whenever you keep `cap_drop: [ALL]`. |

### 2. Start it

In that folder:

```sh
docker compose up -d
```

The same as one `docker run`:

```sh
docker run -d --name composewright --restart unless-stopped \
  -p 127.0.0.1:8484:8484 \
  -v composewright-data:/data \
  -v /var/run/docker.sock:/var/run/docker.sock \
  --read-only --tmpfs /tmp --cap-drop ALL --cap-add CHOWN --cap-add SETGID --cap-add SETUID \
  --security-opt no-new-privileges \
  ghcr.io/aiulian25/composewright:0.1.1
```

On Synology DSM 7.2 or later: **Container Manager → Project → Create**, and paste the compose file above.

## First login

### 3. Get the setup key

composewright creates no account by itself. Until you create the administrator, it prints a setup key in its log at every start. Only someone who can read that log has it, so reaching the page is not enough to claim the app.

```sh
docker logs composewright 2>&1 | grep "setup key"
```

```text
setup key: J7wq…
```

### 4. Create the administrator

Open **http://127.0.0.1:8484**. On **Create the administrator**, paste the setup key, choose a username and a password of at least 12 characters, and press **Create and sign in**. The log also prints a link ending in `/setup?token=…` that fills in the key for you.

composewright sends no email, so there is no reset link: if you forget the password, run `docker exec -it composewright composewright password` on the server.

### 5. Choose how plans are written (optional)

Out of the box composewright uses its own rules and no model, so nothing leaves your machine except fetching the repository and checking its images. To let a model fill in what the rules leave open, add one in **Settings → Model providers** (Ollama, Anthropic, or any OpenAI-compatible server; keys are stored encrypted), then pick it in **Settings → Engine**: *assisted* lets the model fill the gaps, *model* lets it draft the whole plan. Either way it never sees a secret and cannot change images or privileges.

For Ollama on the same machine, use the machine's address, such as `http://192.168.1.10:11434`: inside the container, `127.0.0.1` is composewright itself.

## Your first project

1. **Paste a link.** On **Home**, paste a GitHub repository, for example `https://github.com/miniflux/v2`, and press **Start**. You can also drop files (README, compose, `.env`, Dockerfile) or paste a compose file and `.env` you already have.
2. **Analyse.** composewright fetches the repository as a tarball through GitHub's API (no `git clone`; nothing from it runs) and lists every fact it found with the file and line it came from. Press **Review plan**.
3. **Review.** Check the variables: secrets are generated for you and stay hidden until you reveal them. Choose how the app is reached (a port on this machine, every device on your network, or only through your proxy's network) and where its data lives. If the app asks for a privilege (host network, a device, privileged mode, a host folder, a port below 1024, extra capabilities, or the Docker socket), tick it only if you trust the app: nothing is granted by default, and the test runs without the Docker socket even once you grant it. You can edit `compose.yaml` yourself here too; the policy gate checks your version. Press **Run test**.
4. **Validate.** The stack runs in a throw-away sandbox, with memory, CPU and process limits and ports on loopback only, until every service is healthy; then the sandbox is removed. With a model, a failure is repaired and retried up to three times. Press **Continue to files**.
5. **Deploy it.** On **Result**, download the files as a **.zip** (or copy `compose.yaml` and `.env`), put them in a folder on your server, and run `docker compose up -d` there. `DEPLOY.md` in the zip lists what is left for you, such as a proxy host to add.

**Save files** writes the same files inside composewright's data folder, under `/data/exports/<project>`. With a host data folder they appear on the host at `<your folder>/exports/<project>`; set `COMPOSEWRIGHT_RUN_AS` to your own `uid:gid` before the first start so they belong to you. To save straight into another app's folder, such as Arcane's projects, mount that folder into composewright and choose it under **Another folder**.

<table>
  <tr>
    <td><img src=".github/assets/screenshots/analyse.png" alt="Analyse: the rules that ran and every fact found, with the file and line it came from"><br><b>Analyse</b>: every fact with its file and line</td>
    <td><img src=".github/assets/screenshots/review.png" alt="Review: where each variable came from, generated secrets hidden, and the source of every compose line"><br><b>Review</b>: sources, hidden secrets, your grants</td>
  </tr>
  <tr>
    <td><img src=".github/assets/screenshots/validate.png" alt="Validate: miniflux and its database healthy in the sandbox, with their logs"><br><b>Validate</b>: the stack proven in a sandbox</td>
    <td><img src=".github/assets/screenshots/result.png" alt="Result: save, download or copy the tested files, with every version kept"><br><b>Result</b>: save, download or copy</td>
  </tr>
</table>

The same from the command line, inside the container:

```sh
docker exec composewright composewright gen https://github.com/miniflux/v2
docker exec composewright composewright show miniflux
docker exec composewright composewright export miniflux --to /data/exports/miniflux
```

## Environment variables

None are required. The image sets these, and you can override them under `environment:`:

| Variable | Default in the image | What it does |
|---|---|---|
| `COMPOSEWRIGHT_RUN_AS` | `65532:65532` | The `uid:gid` composewright runs as once started, which also owns its data folder. Set your own, such as `1000:1000`, to own exported files on the host; set it before the first start, or `chown -R` the data folder to it. Root is refused. |
| `COMPOSEWRIGHT_DATA_DIR` | `/data` | The data folder: database, master key, projects, exports and backups. |
| `COMPOSEWRIGHT_CONFIG` | `/data/config.yaml` | The settings file. |
| `COMPOSEWRIGHT_MASTER_KEY_FILE` | `/data/keys/master.key` | The key that encrypts every stored secret. Point it at a file outside the data folder, such as a Docker secret, to keep the key and the data apart. |
| `DOCKER_HOST` | the mounted `/var/run/docker.sock` | Another Docker daemon, such as `tcp://socket-proxy:2375` in the hardened compose file. |
| `HTTPS_PROXY`, `NO_PROXY` | not set | An outbound proxy for GitHub, registries and cloud models. |
| `NO_COLOR` | not set | Plain command-line output. |

Secrets can come from the environment as well: point a setting at `env:<VARIABLE>` and pass that variable to the container, for example `llm.api_key_ref: env:ANTHROPIC_API_KEY` or `scm.github_token_ref: env:GITHUB_TOKEN`.

## Settings

Everything else is a setting: change it in **Settings** in the web UI, or with `docker exec composewright composewright config set <key> <value>`. `docker exec composewright composewright config show` prints every key with its value. The ones people change most:

| Setting | Default | What it does |
|---|---|---|
| `llm.engine` | `rules` | Who writes plans: `rules`, `assisted` or `model` (Settings → Engine) |
| `llm.provider`, `llm.base_url`, `llm.model` | `ollama`, `http://127.0.0.1:11434`, `qwen2.5-coder:14b` | The model (Settings → Model providers) |
| `llm.api_key_ref` | `keyring:llm` | The model's API key: stored encrypted (`keyring:<name>`) or read from `env:<VARIABLE>` |
| `scm.github_token_ref` | not set | A GitHub token raises GitHub's API limit from 60 to 5000 requests an hour: `printf %s "$TOKEN" \| docker exec -i composewright composewright config set-secret github`, then set this to `keyring:github` |
| `registries.allow` | official Docker Hub images, ghcr.io, lscr.io/linuxserver, quay.io | Registries that generated stacks may pull from; a repository owner's own namespace is trusted too |
| `profile.networks` | none | Your reverse proxy or tunnel networks, so apps are reached through them (Settings → Environment profile) |
| `profile.storage.style`, `.base_path`, `.owner` | `volumes`, not set, `1000:1000` | Named volumes, or bind mounts under a base path owned by this user |
| `profile.defaults.tz`, `.puid`, `.pgid` | not set, `1000`, `1000` | Time zone and user ids, added to images that document them |
| `docker.validate.timeout`, `.memory_limit`, `.cpus` | `5m`, `2g`, `2` | The sandbox's limits |
| `backup.schedule`, `backup.keep` | `off`, `8` | Automatic encrypted backups (Settings → Backups) |
| `ui.session_ttl`, `ui.theme` | `12h`, `dark` | How long a sign-in lasts, and the theme |

## Reach it from other machines

- **On your network:** change the port line to `"8484:8484"` and open `http://<server address>:8484`. The password then crosses your network over plain HTTP, which is fine on a network you trust.
- **Through a reverse proxy or tunnel** (Nginx Proxy Manager, Traefik, Caddy, Pangolin, Cloudflare Tunnel): remove `ports:`, join the proxy's network, and point the proxy at `http://composewright:8484`. Any host name works, and the session cookie becomes HTTPS-only by itself when the proxy serves HTTPS.

  ```yaml
  services:
    composewright:
      # … as above, without ports:
      networks: [npm_default]
  networks:
    npm_default:
      external: true
  ```

  So that failed sign-ins count against each visitor rather than the proxy, add the proxy's address to the service: `command: ["serve", "--init", "--listen", "0.0.0.0:8484", "--trusted-proxy", "172.18.0.5"]`.

composewright controls Docker, which is root-equivalent on the host: beyond a network you trust, keep it behind HTTPS.

## More ways to run it

| How | Use | Notes |
|---|---|---|
| Hardened | [`deploy/composewright.hardened.yaml`](deploy/composewright.hardened.yaml) | composewright reaches Docker through a socket proxy on an internal network that passes only the calls validation makes. The proxy needs `DOCKER_GID`, the group of `/var/run/docker.sock`, in a `.env` next to the file. |
| Rootless Docker | the compose file above, with your daemon's socket | composewright stays root inside the user namespace, which is your own user, so its data stays yours. `docker exec composewright composewright doctor --setup-rootless` prints the steps for the host. |

The container image is the only distribution. To work on composewright itself, see [CONTRIBUTING.md](CONTRIBUTING.md).

## Update, back up, recover

- **Update:** change the image tag in `compose.yaml` to the new version (or use `:latest`), then run `docker compose pull && docker compose up -d`. Your data stays in its volume or folder.
- **Back up:** **Settings → Backups** keeps encrypted backups of your settings, model keys and every project on a schedule you choose, in the data folder; download one to keep it elsewhere, and restore from a stored or uploaded backup.
- **Forgot the password:** `docker exec -it composewright composewright password`.
- **Check the install:** `docker exec composewright composewright doctor` checks the data folder, the key, Docker and the sandbox.

## Troubleshooting

| You see | Do this |
|---|---|
| **Docker socket not mounted** in the status bar | Add `- /var/run/docker.sock:/var/run/docker.sock` under `volumes:` and start it again. |
| **No access to the Docker socket** | Keep `cap_add: [CHOWN, SETGID, SETUID]`, so that composewright joins the socket's group at start. |
| `needs the CHOWN, SETGID and SETUID capabilities` in the log | Add `cap_add: [CHOWN, SETGID, SETUID]` next to `cap_drop: [ALL]`. |
| `/data belongs to uid … and is not an empty folder` in the log | Mount an empty folder or a new volume, or run the `chown` the message shows. |
| No `setup key` line in the log | An administrator already exists. Sign in, or set a new password with `docker exec -it composewright composewright password`. |

## Security posture

- **Non-root once started.** The container starts as root only to make its data folder its own and join the Docker socket's group, then runs as uid 65532 with no capabilities, needing only the socket and its data folder; a rootless Docker daemon is the recommended sandbox and `doctor` tells you how to set one up.
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
