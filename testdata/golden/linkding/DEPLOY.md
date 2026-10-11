# linkding

linkding is a bookmark manager that you can host yourself.

Upstream: https://github.com/sissbruecker/linkding
Validation: not validated yet

## Start it

In the folder that holds compose.yaml and .env:

    docker compose up -d
    docker compose ps

## Open it

Published on this machine only: http://localhost:9090

## Data

Data lives in named Docker volumes (`docker volume ls | grep linkding`); `docker compose down -v` deletes them.

## After the first start

- LD_SUPERUSER_PASSWORD was generated for you; it is in .env
- linkding: upstream bind mount ./data:/etc/linkding/data is a named volume here (use --storage bind for a host folder)

## Secrets

.env holds the generated secrets in plain text. Keep it private (mode 0600) and out of version control;
composewright keeps an encrypted copy, so `composewright show linkding --reveal` prints them again.
