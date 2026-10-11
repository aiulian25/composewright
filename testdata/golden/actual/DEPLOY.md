# actual-server

Actual is a local-first personal finance tool. It is 100% free and open-source, written in NodeJS, it has a synchronization element so that all your changes can move between devices without any heavy…

Upstream: https://github.com/actualbudget/actual
Validation: not validated yet

## Start it

In the folder that holds compose.yaml and .env:

    docker compose up -d
    docker compose ps

## Open it

Published on this machine only: http://localhost:5006

## Data

Data lives in named Docker volumes (`docker volume ls | grep actual-server`); `docker compose down -v` deletes them.

## After the first start

- actual_server: upstream bind mount ./actual-data:/data is a named volume here (use --storage bind for a host folder)

## Secrets

.env holds the generated secrets in plain text. Keep it private (mode 0600) and out of version control;
composewright keeps an encrypted copy, so `composewright show actual-server --reveal` prints them again.
