# paperless-ngx

Paperless-ngx is a document management system that transforms your physical documents into a searchable online archive so you can keep, well, _less paper_.

Upstream: https://github.com/paperless-ngx/paperless-ngx
Validation: not validated yet

## Start it

In the folder that holds compose.yaml and .env:

    docker compose up -d
    docker compose ps

## Open it

Published on this machine only: http://localhost:8000

## Data

Data lives in named Docker volumes (`docker volume ls | grep paperless-ngx`); `docker compose down -v` deletes them.

## After the first start

- PAPERLESS_SECRET_KEY was generated for you; it is in .env
- webserver: upstream bind mount ./export:/usr/src/paperless/export is a named volume here (use --storage bind for a host folder)
- webserver: upstream bind mount ./consume:/usr/src/paperless/consume is a named volume here (use --storage bind for a host folder)

## Secrets

.env holds the generated secrets in plain text. Keep it private (mode 0600) and out of version control;
composewright keeps an encrypted copy, so `composewright show paperless-ngx --reveal` prints them again.
