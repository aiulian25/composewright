# Changelog

All notable changes to this project are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and the project uses
[Semantic Versioning](https://semver.org/).

## [Unreleased]

### Added

- `composewright init` creates the data directory (0700), the master key
  (`keys/master.key`, 0600) and a commented `config.yaml`; it is idempotent
  and refuses group- or world-readable state.
- `composewright config show|get|set` with strict decoding (unknown keys are
  errors), validation before writing, and comment-preserving edits.
- `composewright version`, and `composewright --version` for the same line.
- One path-resolution function for config file, data directory and key
  file, covered by a precedence matrix test.
- Redaction of secrets in logs: a `log/slog` handler masks secret-named
  attributes and secret assignments, URL passwords, bearer tokens and private
  keys inside any string.
- The intermediate representation (IR) as Go types plus its JSON Schema
  (`internal/ir/schema.json`), written to the intersection of Anthropic,
  OpenAI strict and Ollama structured-output rules; strict decoding rejects
  unknown keys anywhere, and model output is validated against the schema
  before decoding.
- The policy gate: P1–P12 and consistency rules on the IR, and the YAML gate
  (P9a, P13) on the compose model re-parsed with compose-go, including
  lifecycle hooks, provider services, driver options and symlink-resolved bind
  sources. 19 adversarial plans in `testdata/adversarial` are all blocked.
- Hidden `composewright debug policy PLAN.json...` to run the gate on files.
- Encrypted store (SQLite through modernc.org/sqlite, no cgo): secret values
  are sealed with XChaCha20-Poly1305 under AAD `project_id || key || "v1"`,
  the database file is created 0600, migrations back the database up first,
  and deleting a project cascades to its generations, images and secrets.
- `composewright keys status|rotate|purge`: rotation re-seals every value in one
  transaction and keeps the old key under `keys/retired/` until purged; an
  interrupted rotation stays decryptable.
- Hash-chained audit log; editing, reordering or deleting an entry breaks
  verification.
- `composewright config set-secret NAME` stores an API key or token encrypted
  (read without echo), referenced from the config as `keyring:NAME`.
- `composewright list`.
- Fact extraction (`internal/extract`): compose files are recognised by
  content and read twice (YAML nodes for file:line provenance and raw values,
  compose-go for normalised ports, volumes and healthchecks); Dockerfiles
  (final stage, continuations, heredocs), `.env` samples (comments become
  descriptions), README and docs (docker run commands, variable tables and
  lists, image names, default-credential notes, prompt-injection text), code
  environment reads, Helm values and CI workflow image names.
- Safe repository extraction (`internal/fetch`): gzip/tar through `os.Root`,
  only regular files on a whitelist, symlinks, hardlinks and devices skipped,
  escaping names rejected, size, count and expansion limits; the commit id is
  read from the pax global header.
- A pinned corpus of seven repositories (`testdata/corpus`, refreshed with
  `scripts/corpus-refresh.sh`) with golden facts.
- `composewright gen --stop-after facts <dir>` prints what extraction found, with
  secret values redacted.
- The rules engine (`internal/rules`, R1–R6): compose variant selection
  (development, test and add-on files skipped, documented files preferred),
  app detection by the repository's identity, `build:` replaced by the
  published image, bind mounts turned into named volumes (one per folder,
  named after it, so two folders never share a volume), upstream sample
  secrets replaced by generated ones with connection strings rewritten to
  reference them, empty secrets the stack owns (app signing keys, passwords
  of a database it runs) generated while outside credentials are left to
  the user, settings named like secrets (`*_TOKEN_EXPIRES`, booleans) kept
  as written, Dockerfile stage facts (R2), a sidecar and healthcheck
  catalogue (R4, R5), and the documented `docker run` command as a fallback
  (R6). Every value carries provenance and confidence; gaps are listed.
- `composewright gen --stop-after ir` prints the plan; `--upstream` and the
  checkout's origin remote identify a local directory's repository.
- The renderer (`internal/render`): compose.yaml from a typed node tree
  (fixed key order, `no-new-privileges`, limits and log rotation from the
  profile, `${VAR}` references only), `.env` ordered so references are defined
  first and quoted so Compose reads back exactly what was written (fuzzed
  against compose-go's parser), and DEPLOY.md with proxy-side steps.
- The environment profile is applied: `--expose-via local|lan|none|<network>`
  joins the app (only) to an external network from the profile with
  Traefik labels, an NPM `expose:` or a tunnel note; `--storage bind` renders
  bind mounts from the layout template under the base path; TZ, PUID/PGID,
  restart, logging and limits come from the profile defaults.
- The rendered compose.yaml is re-parsed with compose-go and gated again
  (P9a, P13) before anything is stored.
- `composewright gen --no-llm --no-validate` stores the project, the generation
  and the sealed .env; regenerating reuses secrets already stored.
- `composewright show [--file compose|env|deploy|plan] [--reveal]` (reveals are
  audited), `composewright rm`, `composewright export --to DIR|arcane:DIR`
  (Arcane templates carry a `.env.example` without secrets), and
  `composewright profile show|export|import`.
- `internal/netguard`, the only place that builds HTTP clients and dialers:
  per-purpose host allowlists (llm, scm, registry, release) checked before
  every request and redirect, https only except configured private targets,
  and a dialer that refuses loopback, RFC 1918, link-local, CGNAT, reserved,
  documentation, NAT64/6to4/Teredo and IPv4-mapped addresses after DNS
  resolution. `HTTPS_PROXY`/`NO_PROXY` are honoured.
- `composewright gen https://github.com/<owner>/<repo>` fetches the repository
  tarball through netguard (no git), records the commit from the archive, and
  uses `scm.github_token_ref` when set. Without `--ref` it reads the latest
  release, whose compose file matches the image people pull (a default
  branch can describe a version not released yet), else the default branch;
  the tag shows next to the source. Archives up to 1 GB are read, since
  apps that ship sample media or screenshots run to hundreds of megabytes; a
  bigger one fails as too large rather than as a truncated archive.
- `composewright version --check`, the only request about composewright itself.
- Image verification (`internal/images`, go-containerregistry through
  netguard with anonymous auth): a HEAD resolves and pins every allowlisted
  image's digest, and the image config (`--no-label-check` skips this GET)
  feeds rule R3: the image's own healthcheck, exposed ports and volumes, and
  an OCI source-label check against the repository. Missing images are
  denied, unreachable registries only warned about; results are cached for
  24 hours. Images outside the allowlist are never contacted.
- `composewright gen --offline`: composewright itself contacts nothing; a
  sandbox run still lets the Docker daemon pull images (`--no-validate`
  prevents that).
- `make egress` runs gen through a recording proxy and checks every contacted
  host against the allowlist; `make offline` runs init and gen on a local
  directory inside a network namespace with no network.
- `make eval` reports per-engine results on the corpus and writes
  `eval-report.json`; rules alone pass the policy gate for 7 of 7 entries.
- Sandbox validation (`internal/sandbox`): after an explicit approval (the
  prompt, or `--yes`, which is audited) gen runs the stack as a throw-away
  Compose project `cw-<project>-<id>` with an injected override (restart
  "no", only no-new-privileges, memory/CPU/process caps, every published port
  on an ephemeral 127.0.0.1 port, no external networks, bind mounts replaced
  by throw-away volumes), `up --wait --pull always --no-build`, reads
  `ps -a` and `logs`, and always removes it with `down -v --remove-orphans`.
- Verdicts (crash, unhealthy, timeout, start, fatal log line), with the
  sandbox tier, rootless mode and whether the daemon enforces limits
  recorded on every verdict; logs are stored redacted under the project's
  `logs/` directory and DEPLOY.md shows the last result.
- A janitor removes stale `cw-*` sandbox projects before each run.
- Up to `docker.validate.max_iterations` runs; without a repairer (rules
  only) a failure stores the project as `needs_human`.
- `composewright validate <project>` runs the latest generation again.
- `composewright doctor [--json] [--setup-rootless]` checks the data directory,
  the docker CLI and Compose version (2.24.4 or later), the daemon, the
  sandbox tier, resource-limit enforcement, rootless prerequisites and the
  profile's networks, and lists the egress allowlist; the rootless recipe is
  printed, never run.
- `make test-docker` runs five real sandbox scenarios; `EVAL_VALIDATE=1 make
  eval` adds first-run validation results per corpus entry.
- The model as a plug-in (`internal/llm`): Anthropic (structured outputs,
  server-side refusal fallback, a retry with a larger budget when the answer
  is cut off), OpenAI-compatible servers (strict JSON schema) and Ollama
  (schema as `format`, temperature 0), over plain HTTP through netguard.
- `composewright gen --engine rules|assisted|model`. The default, assisted,
  asks the model only for fields the rules left empty or uncertain and
  merges its answer with precedence: images, needs, services and confident
  rule values never change, and model-written values carry model provenance.
  The model engine drafts the whole plan as a baseline. A model that is
  unreachable, refuses, or answers outside the schema leaves the rules' plan
  with a note.
- Prompts carry the facts, the plan and README sections (deployment sections
  first, within `llm.max_input_tokens`) as labelled JSON data, with every
  secret removed; generations record the engine, provider, model and prompt
  hash.
- A model-backed repair after a failed sandbox run (up to
  `docker.validate.max_iterations`): it sees the verdict and at most 8 KiB of
  redacted logs, and may change only commands, healthchecks, ports,
  dependencies and non-secret variables.
- `make eval` reports rules, assisted and model separately when
  `EVAL_PROVIDER` is set, including how many runs fell back to the rules.
- `composewright doctor` reports the model settings and whether its API key is
  stored, without contacting it.
- Design tokens for the web UI (`web/static/tokens.css`): two palettes,
  dark by default, light on request, `system` opt-in, with code and log
  panes dark in both. A test checks WCAG contrast for every foreground and
  background pair in both palettes and that the tokens match the design.
- A layout template with a header theme toggle that remembers the choice per
  browser and keeps working when storage is unavailable.
- `make lint` refuses colour literals and inline styles outside
  `tokens.css`, and any network reference (CDN, web fonts, analytics) in
  browser assets.
- `make docs` writes a CLI reference from the commands' help, and a test
  fails when a command's help cannot be rendered.
- A GoReleaser configuration for the raw binary (linux amd64 and arm64,
  tar.gz, checksums) whose archives are reproducible from the commit
  (`make reproducible`), and `make licenses` for THIRD_PARTY_LICENSES.md.
- CONTRIBUTING.md, SECURITY.md and AI_POLICY.md; the README states the MIT
  licence.
- Foundations for the web UI: database migration 0002 adds users, hashed
  sessions and the redacted facts of each generation (the database is
  backed up before it migrates); `internal/auth` hashes the password with
  Argon2id (19 MiB, 2 passes), issues session and CSRF tokens, and slows
  down repeated failed logins.
- Sandbox runs on Docker Engine 25 and later probe each healthcheck every
  2 seconds while the app starts, for as long as its own interval and
  retries tolerate failures, so a run ends seconds after the app is ready.
- The Validate step says nothing is needed from you, goes on to the files
  by itself when the run passes and points back to the Review when it
  fails; the sandbox is removed either way.
- The web UI is a workspace from the new design: projects in a sidebar
  that open at the step each one is at, the five steps along the top, the
  files on the right with the same command for the CLI, and a status bar
  with Docker, the sandbox, the engine, the hosts composewright may contact
  and the data directory. A warm palette, dark or light; code stays dark.
- Choose who writes new plans: rules only, rules then a model for what they
  leave open, or a model drafting the plan (`llm.engine`, default `rules`),
  from the top bar, the New page or Settings. `gen --engine` defaults to it.
- Edit compose.yaml on the Review page before the test: your version is
  saved as a new one, both policy gates check it and the test runs exactly
  that file. Every line of compose.yaml says where it came from: a rule
  (R1 to R7), your profile (P), the policy gate (G) or you (U).
- Analyse, Validate and Result stay open for a project after it moves on:
  the Analyse page counts the fields each rule filled and lists the files
  read; the Validate page shows the last run's steps, containers and kept
  logs, with error lines marked and an "Errors only" filter, and on a
  failure offers editing compose.yaml, the Review or another run; the Result
  page lists the versions with what each changed. `plan.json` downloads the
  plan.
- Settings are pages for the environment profile, the engine and model,
  appearance and the account, each next to the YAML it writes. Each page
  saves as one form, shows what you typed again when a value is refused,
  and asks before you leave it with unsaved changes; add as many network
  rows as you need.
- A project can join any number of your proxy and tunnel networks: the
  Review lists them as checkboxes next to the port choice (127.0.0.1,
  0.0.0.0 or not published), several networks can be joined by default,
  and `gen --network` (repeatable) does the same from the CLI. Projects
  that joined one network render as before.
- Delete a project from the bin next to it in the sidebar or from the
  Projects list.
- A Home page to start from: one box takes a GitHub link, a folder, files
  you drop or add, or a pasted compose file with its .env lines before or
  after it; pick the engine next to Start, or start from one of the apps in
  composewright's own test set. It lists recent projects with what each
  still needs. The logo, the sidebar and signing in lead there.
- Files you give without a project name are named after the app's image
  (for example ghost), not after the folder they were kept in.
- A network in the environment profile can have a domain, such as
  npm.example.com for Nginx Proxy Manager or newt.example.com for a Pangolin
  tunnel. A project behind it gets https://<project>.<domain> in settings
  such as BASE_URL or DOMAIN, and the Traefik labels, routing notes and
  DEPLOY.md steps use the same name; networks without one keep using the
  domain suffix.
- Editing a saved model connection keeps its service: the other services
  are locked and the server refuses the change, which before turned a
  Claude connection into Ollama under the same name. The add form starts
  with no service chosen, and the engine menu names the connection in use,
  such as anthropic (Anthropic · claude-opus-5-5).
- Compose files under hacks, experiments, fixtures, playground or scratch
  folders are never chosen, so perber/leafwiki is planned from its README
  rather than from a proxy-auth experiment with dex and fixed passwords.
- The app's own listen address from a sample written for the binary, such
  as LEAFWIKI_HOST=127.0.0.1, becomes 0.0.0.0 with a note: inside a
  container a loopback address cannot be reached through its ports or a
  proxy's network. Other services' addresses, such as DB_HOST, stay.
- The app's own first login, such as ADMIN_PASSWORD or a superuser
  password left empty upstream, is generated like its signing keys, so the
  test run starts and DEPLOY.md says where the password is.
- The Result page edits compose.yaml and .env side by side: the Side by
  side button, the handle on the divider or `Ctrl+\` (`Cmd+\` on macOS) folds
  the result panel into a rail of its sections. The editors number lines,
  draw indentation guides, mark lines changed since the saved version and
  keep compose.yaml's provenance badges; Tab inserts two spaces. As you type
  the server checks that the YAML parses, the policy gate passes and every
  ${VAR} is defined in .env, pointing at lines; an edit never grants a
  privilege, which stays on the Review step. Save as vN stores a version
  made by hand (engine manual, never rewritten by a model repair) and Save
  and re-test also runs it. Secrets stay placeholders unless revealed,
  which the audit log records by name only, and an unchanged placeholder
  keeps its stored value. The layout is remembered per browser.
- A failed run's repair asks the model in a small repair schema, only for
  what a repair may change; the whole plan's schema was too large for
  Anthropic to compile ("The compiled grammar is too large").
- Commands, entrypoints and healthcheck tests keep upstream's `${VAR}`
  references instead of their upstream defaults, so `--requirepass
  ${REDIS_PASSWORD}` follows the generated password.
- Proxy detection gives a network shared by several proxies to the one
  named after it (newt's network to Pangolin even when Nginx Proxy Manager
  joins it too), and Settings shows what runs on each listed network, with
  one click to take its kind and provider when the row says otherwise.
- Validation asks the app from another container on the sandbox network
  once every service is up, the way a proxy reaches it: a pinned busybox
  sends a GET to the plan's probe (service, TCP port, path) and any HTTP
  status counts unless the plan lists the expected ones. Nothing answering
  there fails the run as `unreachable`, so an app listening on 127.0.0.1
  inside its container or on another port no longer passes; the Validate
  page shows the probe as a step and points at the app's last lines. A
  probe that cannot run, or a service without an address of its own, is
  noted and never fails the run.
- The container image, the way to run composewright: one multi-stage
  Dockerfile builds the static binary and puts it with the static docker
  CLI and compose plugin on distroless, as uid 65532, with /data (config,
  database, master key, projects) as the only volume and every base image
  pinned by digest. `serve --init` lets it start on an empty volume.
  `deploy/` holds the one-service compose file and the hardened variant
  behind a socket proxy. `make image` builds it, and `make container-smoke`
  (also in CI) starts the UI on an empty volume and runs init, doctor, gen,
  show and export inside it.
- Service names with capitals, such as SnapOtter in an upstream compose
  file, are lower-cased in the plan with every reference to them, instead
  of blocking it; Docker's DNS ignores case, so nothing that reaches them
  breaks. Each policy finding on Review names the line of compose.yaml it is
  about, with a link that opens the editor on that line.
- The composewright logo, the try square, in the topbar, on Home, on the
  sign-in and setup pages, as the browser tab icon and at the top of the
  README. It follows the dark and light themes.
- A new sign-in page: the logo, what composewright does and a drawing of it on
  one side, the form on the other, with a button that shows the password.
  Setup uses the same layout, and neither page fills in a username.
  Forgot password? explains `composewright password`, a new command that sets a
  new password on the machine composewright runs on and signs every browser out.
- The sidebar's project list scrolls on its own, keeping Home, the filter
  and Settings in place, and the open project in view.
- A live run page can no longer get stuck: the server pings every 15
  seconds, and a page that hears nothing for 45 seconds, or whose stream
  closes, reloads and shows where the run is now.
- Settings › Environment profile looks at the Docker host: it lists the
  networks of reverse proxies and tunnels already running there (Nginx
  Proxy Manager, Traefik, Caddy, SWAG, HAProxy, nginx-proxy, Pangolin's
  newt, cloudflared, Tailscale) with an Add button that fills in kind and
  provider, and says whether each listed network exists on this host. One
  that does not is kept as a placeholder for the host you deploy to, and
  `composewright doctor` says the same instead of warning.
- Settings › Model providers sets up a model step by step: choose a service
  (Claude, OpenAI, Ollama on your own machine, Google Gemini's free tier,
  OpenRouter, or any OpenAI-compatible one), follow its guide to the exact
  page where a key is made, with what it costs and what is free, then fill
  in the prefilled address, model and input budget. Several connections can
  be saved, for other services or other accounts, each with its own
  encrypted key (`llm.connections`, `keyring:llm-<name>`); one click
  switches, and the setup in use is kept when another replaces it.
  Removing a connection deletes its key and records that in the audit log
  (`delete_secret`), as storing one is (`set_secret`). The
  Engine page keeps who writes plans and what the network may reach.
- A run that fails as unhealthy or out of time keeps what its healthcheck
  printed last (secrets redacted), and the Validate page shows it under
  Why it failed: the reason the app's own logs rarely give.
- A failed test says why: what that kind of failure usually means and the
  failing service's last log lines. The Review names the last failed test
  with a link to it, Last run shows the newest tested version even after
  the plan changed, and every tested version in the version tables links to
  its run.
- "Just the files, no test run" on the New page: upload or paste a compose
  file, with or without your own `.env`, and go straight to the files, with
  gaps filled from your Settings and secrets generated. A plan the policy
  gate blocks still stops at the Review, which says why.
- A `.env` you upload, paste or keep in a local folder is yours: its real
  secrets are kept and stored encrypted, and only empty values and
  placeholders such as `changeme` are generated. A repository's `.env`
  stays a sample.
- Sandbox runs report live, redacted output: their own progress, the
  `compose up` progress and service logs polled while `up --wait` runs,
  each line once.
- The pipeline reports each stage to an observer, can revise a stored plan
  (values, typed-in secrets that never enter the plan, regenerated secrets,
  new variables, exposure, storage and granted needs), and can approve and
  validate a stored generation with repairs.
- `composewright serve`: the web UI on 127.0.0.1:8484, or for every device
  on the network with `--listen 0.0.0.0:8484` and nothing else: plain HTTP
  works, addresses typed as an IP and the machine's own name are accepted,
  and the first account is created from the page in the first 15 minutes
  after a start (later with the setup link it prints, or after a restart).
  Sign-in uses Argon2id, hashed session cookies with idle and absolute
  expiry (Secure with the `__Host-` prefix whenever the browser is on
  HTTPS), CSRF tokens on every form, Origin and Host checks against
  cross-site requests and DNS rebinding, a strict Content-Security-Policy
  and a login throttle.
- The five-step flow from the design: Source (a GitHub link, a folder on
  this machine, dropped files or a pasted compose file), Analyse (each
  stage and the facts found, live), Review (services, every variable with
  where it came from, editable values, typed-in and regenerated secrets,
  added variables, Expose via, storage, privileges you grant, policy
  findings and trusting an image namespace), Validate (live sandbox output
  and the verdict, with Cancel) and Result (the files with reveal, copy, zip
  download and save to a folder or Arcane templates).
- Projects, a project page with its generations and audit log, deletion,
  and Settings for networks, paths, defaults, the model and network,
  appearance and the password. Every action is audited as `web:<user>`.
- Sign-in pages say so when the connection is plain HTTP from another
  machine, where the password crosses the network unencrypted.
- `make offline` also runs the web UI with no network: setup, every page, a
  rules-only generation from a folder, the file tabs and the download.
- The assisted engine asks the model for a small fill answer with its own
  schema, derived from the plan schema, instead of a whole plan; Ollama
  answers are streamed, so a slow local model is not cut off.
- Fill prompts carry only the facts a fill uses (variables, env samples,
  Dockerfiles, image mentions, `docker run` commands and notes; compose facts
  are already in the rules' plan), the prompt budget counts the schema that
  is actually sent, and Ollama gets a context window sized to each prompt.
- `llm.max_input_tokens` defaults to 8000, which a 7B local model handles;
  longer prompts made `qwen2.5-coder:7b` loop. Raise it for a cloud model.
  It is a hard limit: when a repository's facts and plan alone exceed it,
  the model is not asked and a note says so.
- `make dev` runs the binary from `make build` in a development container
  (`docker-compose.dev.yml`), on port 8484 for every device on the network;
  machine-specific values come from `.env`.
- Hidden `composewright debug prompt <dir>` prints the exact request a model
  would get for a local checkout, without sending it.
- GitHub links work without `https://`.
- The CLI runs with umask 077, so nothing it creates is group- or
  world-readable.
- `make lint test build`, golangci-lint v2 with forbidigo rules that confine
  HTTP clients, dialers and DNS lookups to `internal/netguard` and process
  execution to `internal/sandbox`; licence allowlist check with go-licenses.
- The evaluation corpus grows from 7 to 20 entries: gitea, mealie,
  nextcloud, bookstack, actual, wallabag, homarr, shiori, immich,
  home-assistant, zigbee2mqtt and pi-hole, pinned at their latest release,
  and a hand-written broken README. `corpus.tsv` records each entry's
  expected rules-only verdict and the rules test checks it, so an entry the
  rules cannot plan yet is part of the baseline instead of a failing test.
- Rule R4 adds the database an app needs when its settings leave the
  database host for you to fill in (BookStack's empty `DB_HOST`) and the
  stack has none: the prefix of the settings or the port they set names
  MariaDB 11.8 or Postgres 18, the app's own user, password and database
  settings create them, the host points at the new service, and the app
  waits for it to be healthy. Nothing is added when nothing names the
  database or the app may go without one.
- A `laravel_key` secret generator: `base64:` and 32 random bytes, the
  `APP_KEY` form `php artisan key:generate` writes. Plans generate `APP_KEY`
  this way; BookStack's image halts its start without one.

### Changed

- The license is MIT, not Apache-2.0, with the copyright held by the
  composewright Authors; `NOTICE`, an Apache-2.0 convention, is gone.
- The project is named Composewright; stackwright was a placeholder. The
  command, the Go module, the image (`ghcr.io/aiulian25/composewright`),
  the environment variables (`COMPOSEWRIGHT_CONFIG`,
  `COMPOSEWRIGHT_DATA_DIR`, `COMPOSEWRIGHT_MASTER_KEY_FILE`), the XDG
  folders, the database file, the key file header, sandbox projects
  (`cw-`), their labels and the session cookie carry the new name. A data
  directory made under the placeholder is not read: start over with `init`.
- The logo's wordmark reads composewright in the topbar, on the sign-in
  pages, in the README and in the favicon, which is now the simplified mark
  on a dark tile. Browsers that skip SVG icons get `favicon.ico`, and iOS
  home screens an Apple touch icon.

### Removed

- The `.deb`, `.rpm`, AppImage and raw-binary distributions: the container
  image is the only way to run composewright, and `make build` stays for
  development. Gone with them: GoReleaser and its release archives, the man
  pages and path resolution's `$STATE_DIRECTORY`, `$CONFIGURATION_DIRECTORY`
  and `/etc/composewright/config.yaml`.

### Fixed

- Plans come from the compose file a deployment uses, found by growing the
  corpus: files that mount the repository (development), staging files,
  proxy examples and files that only build another program in the
  repository are skipped. A service's bare local build tag such as
  `mealie:dev` becomes the image the project documents, stable tags before
  nightly ones. The documentation fallback also takes the app's image from
  outside the owner's namespace and matches names without separators
  (pi-hole publishes pihole/pihole). A compose file with an empty key, such
  as an `environment:` holding only comments, is no longer dropped, and a
  Dockerfile stage built `FROM ${ARG}-…` inherits the stage the ARG's
  default names. On the corpus, the rules engine's first-run validation
  passes go from 8 to 12 of 20.
- A service that loads an env_file gets the settings it shares with other
  services, such as immich's `DB_PASSWORD`, which the database receives as
  `POSTGRES_PASSWORD: ${DB_PASSWORD}`; variables that only configure the
  compose file (a host port, a folder, an image tag) stay out.
  `example.env`, `sample.env` and `env.example` count as samples of `.env`.
- Postgres 18 and later keep their data under `/var/lib/postgresql` and
  refuse a volume at the old `/var/lib/postgresql/data`. A volume upstream
  wrote for the old path is mounted at the new one for those versions
  (`latest` included), unless the service sets `PGDATA`, with a note in the
  plan; nextcloud's example uses `postgres:latest`.
- In a monorepo each service takes the facts of the Dockerfile in the folder
  named after it: immich-server got the machine-learning image's Python
  healthcheck, which it cannot run, and never became healthy.
- The profile's default memory limit (512m) is a floor that a plan raises
  for images known to need more, never lowers. The catalogue gives immich's
  server, machine learning and Postgres 2g each, per its requirements (6 GB
  in all, 2 GB for Postgres under Docker limits); at 512 MB the server's
  first geodata import ran out of heap.
- A README `docker run` whose flags carry a `` `#optional` `` comment, as
  every linuxserver.io README does, is read whole; the comment had been
  taken for the image name, so the plan came from a one-off helper command.
- The reachability probe stops at the first HTTP status line. When an app
  redirected to its public address, which the sandbox network refuses,
  wget's "Connection refused" for the redirect kept it asking until the
  validation timeout ran out: BookStack passed only after five minutes.
- `gen` on a local folder no longer stops at the first file it may not
  read, such as a checkout's 0600 files mounted into the container: files
  and folders it cannot read are left out, and a note names them. Problems
  extraction records, such as a compose file that does not parse, are now
  notes of every run, not only of `--stop-after facts`.
- A compose file whose only service is a proxy image such as nginx is
  planned, with that service as the app; it was skipped as an add-on, so the
  plan came out empty. A file with only databases and caches is still an
  add-on.
- A compose variable written with two different defaults gets the first one
  in the file. Before, the pick could change from run to run, so the same
  repository could produce a different `.env`.

### Security

- Builds use Go 1.26.9 or later: `go.mod` names it as the toolchain, so an
  older installed Go fetches it. With Go 1.26.5, `make vuln` found 19
  reachable standard-library vulnerabilities (net/http, html/template,
  crypto/tls, net/url, os, encoding/asn1, net/textproto); it now finds none.
  The release image already built with Go 1.27.2. `make lint` and
  `make licenses` pass the active toolchain's GOROOT to go-licenses, which
  otherwise mistakes the standard library of a switched toolchain for
  packages without a module.
