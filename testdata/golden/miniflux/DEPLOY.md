# miniflux

Miniflux is a minimalist and opinionated feed reader.

Upstream: https://github.com/miniflux/v2
Validation: not validated yet

## Start it

In the folder that holds compose.yaml and .env:

    docker compose up -d
    docker compose ps

## Open it

Published on this machine only: http://localhost:8080

## Data

Data lives in named Docker volumes (`docker volume ls | grep miniflux`); `docker compose down -v` deletes them.

## After the first start

- ADMIN_PASSWORD was generated for you; it is in .env
- POSTGRES_PASSWORD was generated for you; it is in .env
- miniflux: upstream publishes host port 80; a port above 1024 is used so no extra privilege is needed

## Secrets

.env holds the generated secrets in plain text. Keep it private (mode 0600) and out of version control;
composewright keeps an encrypted copy, so `composewright show miniflux --reveal` prints them again.
