# Changelog

All notable changes to this project are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and the project uses
[Semantic Versioning](https://semver.org/).

## [Unreleased]

## [0.2.0] - 2026-10-18

Generated stacks can now pass secrets as files, carry the config files their
mounts need, and be checked for the platform they will run on; repositories
can come from GitLab, Codeberg and other Gitea or Forgejo hosts. A failed test
run can be repaired by the model on demand, test runs show each service's
memory and CPU, and Settings gains Maintenance and Advanced pages.

### Added

- After a failed test run, the Validate page offers Ask the model whenever a
  model is set up, whichever engine made the project. The model reads the
  run's verdict and logs, never a secret, and its proposal is shown before
  anything changes: the lines it changes in compose.yaml and .env are marked
  M, with what it was not allowed to change, its notes and the policy gate's
  answer. Apply and re-test stores exactly that proposal as the next version,
  without asking the model again, and runs the test again; Dismiss leaves
  the project as it was. A version whose compose.yaml
  you edited shows the proposal but is never repaired, since a repair
  renders compose.yaml from the plan. `composewright validate --repair` does
  the same from the command line, asking before it applies. Every time the
  model is asked, the audit log records it with a hash of what it was sent.
- Test runs now measure how much memory and CPU each service uses while it
  starts and once every service is up. The Validate page's Containers list
  and `composewright validate` show each peak against the service's memory
  limit, such as peak 612MiB of 1GiB. After a passing run the Review page
  warns when a limit is below one and a half times the peak, where OOM kills
  are likely, with a button that makes a new version with a limit that fits,
  and also when a limit is more than four times what was used. Peaks are
  measured even on a daemon that does not enforce limits, and a run never
  fails because they could not be measured; a slow daemon delays it by ten
  seconds at most.
- A host port another project already publishes, or a running container on
  the Docker host already binds, is now a P4 warning, as "host port 8080 is
  also published by project linkding (v3)". The Review page offers Use 8081,
  the next port nothing takes, which publishes the app there as a new
  version; `composewright gen` prints the same warning, and
  `--port SERVICE:CONTAINER=HOST` chooses a host port from the command line.
  A regeneration keeps the chosen port. The project's own deployed
  containers and sandbox runs are left out, a Docker daemon that does not
  answer never holds up a plan, and `gen --offline --no-validate` does not
  ask Docker at all.
- Expose via on the Review page has a Public address field: a host name such
  as rss.example.com, or one with a path such as home.example.com/rss. It
  replaces the derived <project>.<domain> in the Traefik rule, the proxy
  steps in DEPLOY.md and URL variables such as BASE_URL, also when the app
  joins no network. A path adds a PathPrefix rule and strips the prefix
  before the app. Blank keeps the derived host; a regeneration keeps the
  address, and `composewright gen --public-host` sets it from the command
  line.
- A compose example written in a fenced yaml or untyped block of a README or
  docs page is now read as an upstream compose file, named after the page and
  the line the block opens on, such as README.md#L42. A plan built from it
  cites those lines with high confidence instead of falling back to a docker
  run line or the Dockerfile. A compose file the repository ships for
  deployment always wins over such an example, and an .env sample's value
  over one the example gives. The Analyse page counts these examples apart
  from the compose files.
- Two new Settings pages cover what used to need the CLI. Maintenance shows
  the master key's id, age and how many values it encrypts, rotates it or
  purges retired keys after asking for your password (not while a run is
  going, and the running server uses the new key at once), checks GitHub
  Releases for a new version when you ask, and runs doctor's Docker checks
  in the browser. Advanced edits the sandbox tier (runsc included), the
  sandbox limits, timeout and repair rounds, the Docker host and CLI path,
  the code hosts and the session length, and stores or removes a GitHub
  token, encrypted like model keys. The Environment profile page downloads
  and imports `profile.yaml`, as `composewright profile export` and
  `import` do.
- A GitHub repository whose tarball is past the size or file-count limits
  is no longer refused: composewright lists its files through the Git trees
  API and downloads only the ones it reads, each within its size limit and
  50 MB in all. Files come from raw.githubusercontent.com, now in the
  default `scm.allow_hosts`, up to 2000 of them; an install whose list
  lacks it uses the GitHub API, one request per file, up to 400 and within
  the hourly limit left. README, compose, Dockerfile, env samples and docs
  are always downloaded and the run stops with a clear message when they
  alone pass that; source files fill what is left and the rest are skipped.
  The Fetch step says how many files it fetched this way and how many
  source files it skipped.
- What composewright knows about images, such as the healthcheck Miniflux
  gets or the memory Immich needs, can now be added to without a new
  release: `.yaml` files in the `catalogue.d` folder of the data directory,
  `/data/catalogue.d` in the container, use the same `families:` and
  `apps:` lists as the built-in catalogue. They are read in name order and
  a later file wins: a family replaces the one of its name and an app entry
  wins for the images it lists. A healthcheck from them is cited as R5, as
  the built-in ones are. Commands read the files each time and the web UI
  when it starts and after a restore. Settings › Advanced lists them with
  how many families and apps each adds and the folder they are read from;
  a file that is not valid YAML, or whose image, `memory_limit` or
  healthcheck cannot be used, is listed there with its problem and left
  out, `composewright doctor` reports it in a catalogue overrides row, and
  `composewright gen` notes it. Backups include the files and a restore
  writes them back.
- Repositories on GitLab, Codeberg and any other GitLab, Gitea or Forgejo
  host can now be generated from as GitHub ones are: paste
  `https://gitlab.com/<group>/<project>` or
  `https://codeberg.org/<owner>/<repo>` once the host is in
  `scm.allow_hosts` (Code hosts under Settings › Advanced); a host left out
  is refused by the egress guard, by name. The latest release, else the
  default branch, is fetched as one tarball with the same size limits, and
  generating a project again reads the same commit. github.com, gitlab.com
  and codeberg.org are known by name; any other host is asked once whether
  it runs Gitea or GitLab, and one listed in
  `network.allow_private_targets`, such as a Gitea on your network, can be
  read over plain HTTP. Private repositories take a token from
  `scm.token_refs`, such as `{gitlab.com: keyring:gitlab}`. The owner's own
  image namespaces are trusted as a GitHub owner's are, and the Analyse
  page names the source as codeberg.org/owner/repo @ commit.
- Try it on the Model providers page sends a connection one tiny request and
  says whether it answered: the model name it answered as, the tokens in and
  out and the time, or the provider's error with the key left out. It works
  on each saved connection and on the add or edit form before saving; a key
  typed there is used for that one request and kept nowhere.
  `composewright doctor --contact-model` does the same for the model in use;
  plain `composewright doctor` still never contacts the model.
- An image namespace blocked by the registry allowlist can now be trusted for
  good from the Review page: Always trust adds it to `registries.allow`, as
  Settings › Engine would, and gates the plan again, so later projects from
  the same namespace are not blocked. Trust for this project still trusts it
  for that project only.
- The reachability check now asks the app for its health endpoint when the
  evidence names one: the URL its healthcheck asks on the probed port, the
  image's own HEALTHCHECK, a known endpoint for images such as miniflux, or a
  health path the README documents. It then expects a success or a redirect,
  unless the healthcheck itself accepts any answer or logs in, so an error
  there fails the run as unreachable, and the Validate page says what was
  asked and what answered. Without such evidence it asks / and takes
  any answer, as before.
- A run that failed because of a log line now offers Not a failure for this
  app next to each line that failed it. The line's text, without the
  container name and time, is kept as normal for that project and the test
  runs again; a matching line never fails its runs after that. The Review
  page lists these under Ignored log lines, where each one can be removed,
  and they travel with the project in backups.
  `composewright validate <project> --ignore-log 'pattern'` does the same from
  the command line with a regular expression over that same text.
- Settings › Audit log lists every audit entry, newest first, including
  sign-ins, settings changes, backups and secrets set outside any project.
  Each row opens to show the details recorded with it, which may name a
  secret but never hold its value. Filter by action or project, check the
  hash chain with Verify chain, or download the entries as JSON Lines, which
  is recorded too. `composewright audit list [--action X] [--project Y]`
  prints the same list.
- Lines in a repository that speak to a language model, such as "ignore
  previous instructions", are now listed with their file and line on the
  Analyse and Review pages instead of only being counted, and a note for
  deployment that repeats one is marked.
- A repository downloaded as one `.zip` or `.tar.gz` can be dropped on Home or
  New as it is. It is unpacked with the same limits and path checks as a
  GitHub tarball, keeping the same files, and the Fetch step says how many
  files it held and how many were kept. An archive with a path that leads
  outside it is refused, links in it and the `__MACOSX` folder Finder adds
  are skipped, one without a single folder for the repository is refused
  with a plain message, and other kinds such as `.7z` or `.rar`, or an
  archive given with loose files, are refused with a message saying what to
  give instead. `composewright gen ./repo.zip` reads one the same way.
- `profile.registry_mirror` now works: set it to a mirror such as
  `mirror.gcr.io` or your own pull-through cache, as Registry mirror under
  Settings › Environment profile or with
  `composewright config set profile.registry_mirror mirror.gcr.io`, and
  Docker Hub images are checked through it instead of Docker Hub. The plan
  still names and pins the `docker.io` image, the mirror joins the hosts
  composewright may contact, and a mirror on your network over plain HTTP
  needs its host:port in `network.allow_private_targets`. When Docker Hub
  itself refuses a check for its anonymous pull limit, the warning now says
  to set this key or wait.
- `docker.sandbox_tier: runsc` now runs every test-run container, and the
  reachability probe, under gVisor: set it with
  `composewright config set docker.sandbox_tier runsc`. The Docker daemon
  must offer the runsc runtime; when it does not, `composewright doctor`
  fails the sandbox tier check, the status bar says so and a test run
  refuses to start instead of running without gVisor.
  `composewright doctor --setup-runsc` prints the steps to install it on the
  host. The Validate page lists the runtime among the sandbox limits, and the
  generated `compose.yaml` stays the same.
- Services the upstream compose file starts only with a Compose profile, such
  as an optional text-to-speech or GPU service, no longer vanish. The Analyse
  page lists each as an optional service with its profile, and the Review page
  has an Optional services card where each can be ticked. They are off by
  default. Ticking or unticking one and applying generates the project again
  from the same repository commit or folder, with the ticked services, and the
  optional services they depend on, in `compose.yaml` and the test run. Files
  you uploaded or pasted are kept only for their run, so the card shows their
  optional services without letting you switch them. A model never sees a
  service that is off. `composewright gen --enable-optional <name>` does the
  same from the command line, and a later `gen` keeps the choice. Generating
  an existing project again now keeps the values you set and the variables you
  added; a password the stack already had keeps its value, and a generated
  password stays stored while its service is off, so switching it on again
  reuses it.
- Limits and hardening that the upstream compose file sets now reach the
  generated `compose.yaml`: `shm_size` as written, such as Immich's 128mb for
  its database; `mem_limit`, `cpus` and `pids_limit`, or the same limits under
  `deploy.resources.limits`, merged with the profile defaults as before, so a
  memory limit below the default still rises to it; and `cap_drop`,
  `read_only` and `tmpfs` as written, with tmpfs volumes among the tmpfs
  mounts. The Review page badges these lines R1 when upstream set them. The
  test run keeps them and lowers a larger `shm_size` to the sandbox memory
  limit.
- Devices, `cap_add`, `privileged: true` and `network_mode: host` in the
  upstream compose file no longer vanish from the plan. Each becomes a
  request on the Review page's Needs card, with the service, the device or
  capability and the file and line upstream wrote it on, and every request
  of one kind is listed, not only the last. Until you grant it the policy
  gate blocks the plan; granted, `compose.yaml` carries it as upstream wrote
  it under a NEED GRANTED BY USER comment, with all the devices or
  capabilities of one service in one list, and DEPLOY.md names each device
  or capability. The Docker socket request names its line too.
  `composewright gen --allow-need devices` grants it from the command line.
  A device given as host:container is now written as given instead of being
  doubled.
- When the repository ships several compose files, such as a plain one and
  Traefik, Caddy or rootless variants, the Review page has a Built from card
  that lists each with its service count and marks the one the plan was built
  from. Choosing another and applying generates the project again from that
  file at the same repository commit or folder, keeping the values you set
  and the stored secrets for variables both files have. Optional services
  the new file also offers stay on; the others go off with a notice, and
  switching optional services later keeps the chosen file. Files you
  uploaded or pasted show the list without letting you switch.
  `composewright gen --compose-file <path>` builds from the file at that path
  in the repository, and a later gen without it keeps that file; a path it
  lacks is shown in the gen output and the deployment notes, and the usual
  file is used.
- Check images on a project's page asks each image's registry again what
  its tag points at, with HEAD requests only so nothing is pulled, and lists
  the newer stable tags of the same family: for 2.2.3, the newer 2.2.x and
  2.x tags, never nightly, dev or release candidates. Offered tags keep the
  pinned tag's variant, so 2.2.3-php7 is offered 2.2.4-php7, not a -php8
  tag. Over a compose.yaml edited by hand it checks the images the edit
  names. A tag that now points at a different image than the one pinned
  says so, and Use 2.2.5 and re-test makes a new version with that tag,
  verified, pinned and gated again, and tests it; the chosen tag stays when
  the project is generated again. `composewright images <project>` prints
  the same table. Each version now keeps its images' labels and build date: the
  Review's compose pane shows the version, build date, revision and source
  when you hover an image line, and the Analysis lists the image version.
- The Environment profile page has a Target platform: linux/amd64,
  linux/arm64, linux/arm/v7 or this machine's, kept as `profile.platform`.
  Every image is checked for a variant for it, and one without is a P2 deny
  that names what the image has, such as "has no linux/arm/v7 variant (it
  has linux/amd64, linux/arm64)", until another tag or image is chosen. The
  pinned digest stays the image index's, so compose.yaml runs on every
  platform it lists, while labels and healthchecks are read from that
  platform's image. DEPLOY.md says "verified for linux/arm64" once every
  image has the variant, and a test run says when the sandbox tests its own
  platform instead.
- A generated or typed-in secret whose `*_FILE` variant the repository
  documents, such as POSTGRES_PASSWORD_FILE, is passed as a Compose secret
  file instead of an environment value: compose.yaml mounts
  `secrets/postgres_password` and sets POSTGRES_PASSWORD_FILE to
  /run/secrets/postgres_password, and .env only says where it is. This
  happens only when every service reads the secret under that name and no
  connection string, command or healthcheck reads its value. The Review
  page marks it "as a file", the export, the Result page's Save, the zip
  and the shell commands write the file with mode 0600 in a 0700 folder,
  test runs use the same file, and an Arcane template leaves it out.
  `composewright show --file secrets` lists these files, values only with
  `--reveal`. The shell copy writes each secret with printf, so no newline
  ends up in a password, and `composewright gen` now counts automatic fixes
  as "fixed automatically".
- A file of the repository that upstream's compose file bind-mounts, such
  as `./nginx.conf:/etc/nginx/nginx.conf`, now travels with the stack
  instead of being left to you: it is kept beside compose.yaml as
  `config/nginx.conf` and mounted read-only. When the repository ships only
  an example of it, such as `configuration.example.yaml` for
  `configuration.yaml`, the example is used. The Review page shows each
  such file in a tab of its own, DEPLOY.md lists them, and the export, the
  Result page's Save, the zip and the shell commands write them readable
  by the container's user, Arcane templates included. Test runs mount the
  same copy. The files are stored with each version encrypted with the
  master key, a key rotation moves them to the new key, backups carry
  them, and generating the project again reads them anew. Fetches now keep
  `.conf`, `.toml`, `.ini`, `.cfg` and `.properties` files and Caddyfiles
  so they can be bundled.

### Changed

- The Result page finishes a project in one card: save the files to a folder,
  then start them with the command shown, or download a .zip for another
  machine. Change opens a short list of folders; saving over files from an
  earlier save asks first instead of needing a checkbox. Copying moves into
  the file panel's Copy menu, and Edit and re-test opens compose.yaml and .env
  side by side on the page. Start over and Delete project left the page: the
  side panel's New project and delete buttons do both.
- Once a version passes its test, the untested versions before it are removed,
  so History lists what was tested and the work since; the audit log counts
  them.

### Fixed

- An app port that the upstream compose file writes without a host side,
  such as `"80"`, is now published on a port of its own, as Compose would
  publish it; a database or cache written that way stays inside the stack.
  `DEPLOY.md` no longer sends you to port 0 when nothing is published.
- An address with no place for a credential under a secret name, a host
  alone such as MAILER_DSN=smtp://127.0.0.1 or a local path such as
  sqlite:///data/app.db, is kept as written. It used to be replaced by a
  generated string, which no app reads as a mailer or database address.
- A password typed into a value shown as plain text, on the Review page or
  in the editor, such as a mailer address with a user and password, is
  stored sealed as a secret and kept out of the plan.
- The file panel's buttons wrap onto a second row instead of running off a
  narrow panel.
- The healthcheck an app's image ships with no longer blocks the stack: one
  read from the image itself, such as a probe of the address the app binds,
  passes the rule that a healthcheck may only contact its own container,
  since Docker runs it whether or not compose.yaml repeats it. Healthchecks
  from documentation or a model are held to the rule as before.
- A variable upstream's compose file leaves empty, such as
  `DATABASE_URL: ${DATABASE_URL:-}`, stays empty unless the `.env` sample
  beside that compose file sets it. A commented-out example for an outside
  database, or a subproject's development sample, no longer points the app at
  a database that is not in the stack.
- A connection string to a database outside the stack always reads the
  password you are asked for. When it was the app's only variable, `.env`
  kept upstream's example password in it instead.
- An example password upstream writes as a variable's default in a compose
  file, as in `${DB_PASSWORD:-changeme}`, is no longer shown on the Analyse
  page or sent to a model.
- Applying the Review page no longer keeps a URL variable such as BASE_URL as
  your own value only because the page sent back the URL it showed. One kept
  that way before follows the public address and networks again.

## [0.1.3] - 2026-10-12

Generated stacks get each service a password of its own, the paths the image
really uses, and the settings documented anywhere in a repository; the model
that fills the gaps now fits a small local model.

### Changed

- The model that fills what the rules leave open is sent only the facts the
  asked fields use: the variables it should describe, and the image facts
  when it is asked about a service. The plan it reads leaves out the optional
  settings, which it cannot change, and repeats no description. A repository
  that documents hundreds of settings now usually fits a small local model's
  8000-token budget instead of skipping the model.

### Fixed

- Two services whose passwords upstream sets by separate names, such as
  `${REDIS_PASSWORD:-app}` and `${POSTGRES_PASSWORD:-app}`, no longer share one
  generated secret because their placeholder defaults match: each gets its own.
- A commented-out setting in `.env` shows the value the app's image sets, such
  as `/data/files`, instead of a sample's path for running without Docker,
  which pointed outside every volume.
- Documentation kept in a docs folder at any depth, as in a monorepo, is read
  for settings, without its translated copies. A table without a name column
  counts only UPPER_SNAKE names, so lists of file formats or HTTP methods are
  no longer taken for variables, and a default such as "(empty)" means none.
- Descriptions from `.env` samples: an inline comment describes its entry; a
  short section title over several entries, a note that starts with another
  variable, and advice for upstream's development setup no longer describe the
  next entry; `KEY=  # note` has no value, and a commented sentence such as
  `# X=false is honored as an alias` is not a setting.

## [0.1.2] - 2026-10-12

Generated stacks now list every setting an app documents, ready to switch on.

### Added

- Generated stacks list the settings an app's upstream documents, in a `.env`
  sample or a README or docs table, and the plan leaves unset: `.env` shows
  each one commented out with its description, default and source, and
  compose.yaml passes it to the app by name, so removing the `# ` in `.env` is
  all it takes. While commented out, the image's own default applies. Example
  secrets from upstream are never copied, and variables for tests, CI or a
  development setup are left out. Review lists them under the environment.

## [0.1.1] - 2026-10-11

Fixes from the first install on a server: composewright prepares its own
data folder, answers every host name, asks for a setup key, and says why
Docker cannot be used; generated stacks can get the Docker socket when you
grant it.

### Added

- A `docker_socket` need. An app whose upstream mounts the Docker socket
  (Portainer, Dockge, Traefik's Docker provider, composewright itself) asks
  for it instead of losing the mount, and once you grant it on Review or
  with `--allow-need docker_socket`, compose.yaml mounts
  `/var/run/docker.sock` marked NEED GRANTED BY USER. The sandbox test still
  runs without it.
- `COMPOSEWRIGHT_RUN_AS` picks the `uid:gid` the container runs as, so
  exported files can belong to you on the host; root is refused.

### Changed

- The image starts as root only long enough to make its data folder its own
  and join the Docker socket's group, then runs as uid 65532 with no
  capabilities. A compose file that drops every capability needs
  `cap_add: [CHOWN, SETGID, SETUID]`; `group_add` and `DOCKER_GID` are no
  longer needed. Under rootless Docker it stays root, which is your own user.
- The web UI answers to every host name, so a reverse proxy or the
  machine's own name works from the first visit. `serve --allow-host` is
  accepted and ignored.

### Fixed

- Generated deploy notes give bind-mounted folders to the user an image
  runs as when it declares a fixed one, instead of always the profile's
  owner, which left such an app unable to write its own data.
- When Docker cannot be used, the status bar, Home and `doctor` say why (the
  socket is not mounted, cannot be opened, or nothing answers on it) and
  what to change, instead of only "Docker unreachable".
- A data folder Docker created as root for a bind mount, or an empty one you
  made, no longer stops composewright from starting: it becomes the app's,
  with mode 700. A folder that already holds someone else's files is refused
  with the command that fixes it.
- `composewright gen` no longer says the images it pulls for the sandbox
  are removed afterwards: the sandbox and its volumes are, and the images
  stay for the real deployment.

### Security

- Creating the first account needs the setup key composewright prints in its
  log at every start until an administrator exists. Reaching the setup page
  in the first 15 minutes is no longer enough, which left an install behind a
  public domain open to anyone during that time.

## [0.1.0] - 2026-10-11

The first public release.

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
  a new master key, and nothing composewright writes is readable by other
  users. Actions are recorded in a hash-chained audit log that
  `audit verify` and `doctor` check: editing, reordering or removing an
  entry other than the newest breaks the chain.
- **Backups.** Settings → Backups keeps backups of the Settings choices,
  the model API keys they use and every project, with its versions,
  compose.yaml, DEPLOY.md and `.env` secrets: made automatically (weekly,
  monthly or every few days, keeping as many as you choose) or when you
  ask, and encrypted with a backup passphrase you set once (Argon2id and
  XChaCha20-Poly1305), so a downloaded copy restores on any install.
  Downloading and restoring, from a stored backup or an uploaded file, ask
  for the account password again; a restore keeps the projects that exist
  unless told to replace them, and everything restored is checked again
  before it runs.
- **Egress control.** composewright's own connections all go through
  `internal/netguard`: each purpose has a host allowlist, and internal
  addresses are refused after DNS resolution unless configured as private
  targets. A model is reached over plain HTTP only at a local address (a
  private or Tailscale IP, or a LAN name), so an API key never crosses the
  internet unencrypted. `gen --offline` keeps composewright itself off the
  network.
- **Commands:** `init`, `config`, `keys`, `audit`, `gen`, `list`, `show`,
  `rm`, `export` (to a folder or Arcane templates), `profile`, `validate`,
  `doctor`, `serve`, `password` and `version`.
- **The container image**, the way to run composewright: distroless, uid
  65532, with the docker CLI and compose plugin, `/data` as the only volume
  and every base image pinned by digest. `deploy/` has a one-service compose
  file and a hardened variant behind a Docker socket proxy.
- **Development:** `make lint test build`, `make eval` on 20 pinned corpus
  entries, and `make test-docker`, `container-smoke`, `egress`, `offline`,
  `vuln`, `licenses` and `docs`.
- MIT licence, with CONTRIBUTING.md, SECURITY.md and AI_POLICY.md.
