# navidrome

Navidrome is an open source web-based music collection server and streamer. It gives you freedom to listen to your

Upstream: https://github.com/navidrome/navidrome
Validation: not validated yet

## Start it

In the folder that holds compose.yaml and .env:

    docker compose up -d
    docker compose ps

## Open it

Published on this machine only: http://localhost:4533

## Data

Data lives in named Docker volumes (`docker volume ls | grep navidrome`); `docker compose down -v` deletes them.

## Secrets

.env holds the generated secrets in plain text. Keep it private (mode 0600) and out of version control;
composewright keeps an encrypted copy, so `composewright show navidrome --reveal` prints them again.
