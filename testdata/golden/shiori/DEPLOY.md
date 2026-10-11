# shiori

Check out our latest Announcements

Upstream: https://github.com/go-shiori/shiori
Validation: not validated yet

## Start it

In the folder that holds compose.yaml and .env:

    docker compose up -d
    docker compose ps

## Open it

Published on this machine only: http://localhost:8080

## Data

Data lives in named Docker volumes (`docker volume ls | grep shiori`); `docker compose down -v` deletes them.

## After the first start

- Since this is our first time, we don't have any account registered yet. With that said, we can use the default user to access web interface: (docs/Usage.md:91)

## Secrets

.env holds the generated secrets in plain text. Keep it private (mode 0600) and out of version control;
composewright keeps an encrypted copy, so `composewright show shiori --reveal` prints them again.
