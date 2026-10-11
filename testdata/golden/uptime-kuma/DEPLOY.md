# uptime-kuma

Uptime Kuma is an easy-to-use self-hosted monitoring tool.

Upstream: https://github.com/louislam/uptime-kuma
Validation: not validated yet

## Start it

In the folder that holds compose.yaml and .env:

    docker compose up -d
    docker compose ps

## Open it

Published on this machine only: http://localhost:3001

## Data

Data lives in named Docker volumes (`docker volume ls | grep uptime-kuma`); `docker compose down -v` deletes them.

## After the first start

- uptime-kuma: upstream bind mount ./data:/app/data is a named volume here (use --storage bind for a host folder)

## Secrets

.env holds the generated secrets in plain text. Keep it private (mode 0600) and out of version control;
composewright keeps an encrypted copy, so `composewright show uptime-kuma --reveal` prints them again.
