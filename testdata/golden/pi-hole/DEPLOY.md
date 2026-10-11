# docker-pi-hole

Using Docker-compose:

Upstream: https://github.com/pi-hole/docker-pi-hole
Validation: not validated yet

## Start it

In the folder that holds compose.yaml and .env:

    docker compose up -d
    docker compose ps

## Open it

Published on this machine only: http://localhost:8053

## Data

Data lives in named Docker volumes (`docker volume ls | grep docker-pi-hole`); `docker compose down -v` deletes them.

## Secrets

.env holds the generated secrets in plain text. Keep it private (mode 0600) and out of version control;
composewright keeps an encrypted copy, so `composewright show docker-pi-hole --reveal` prints them again.
