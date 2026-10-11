# vaultwarden

An alternative server implementation of the Bitwarden Client API, written in Rust and compatible with official Bitwarden clients disclaimer, perfect for self-hosted deployment where running the offic…

Upstream: https://github.com/dani-garcia/vaultwarden
Validation: not validated yet

## Start it

In the folder that holds compose.yaml and .env:

    docker compose up -d
    docker compose ps

## Open it

Published on this machine only: http://localhost:8000

## Data

Data lives in named Docker volumes (`docker volume ls | grep vaultwarden`); `docker compose down -v` deletes them.

## Optional settings

.env lists 145 settings the upstream documents, commented out, such as ADMIN_RATELIMIT_MAX_BURST, ADMIN_RATELIMIT_SECONDS, ADMIN_SESSION_LIFETIME, ADMIN_TOKEN, ALLOWED_CONNECT_SRC. To use one, remove the "# " before it in .env and run `docker compose up -d` again.

## Secrets

.env holds the generated secrets in plain text. Keep it private (mode 0600) and out of version control;
composewright keeps an encrypted copy, so `composewright show vaultwarden --reveal` prints them again.
