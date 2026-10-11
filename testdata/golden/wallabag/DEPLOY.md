# docker

With this application you will not miss content anymore. Click, save, read it when you want. It saves the content you select so that you can read it when you have time.

Upstream: https://github.com/wallabag/docker
Validation: not validated yet

## Start it

In the folder that holds compose.yaml and .env:

    docker compose up -d
    docker compose ps

## Open it

Published on this machine only: http://localhost:8080

## Data

Data lives in named Docker volumes (`docker volume ls | grep docker`); `docker compose down -v` deletes them.

## After the first start

- Default login is wallabag:wallabag. (README.md:13)

## Secrets

.env holds the generated secrets in plain text. Keep it private (mode 0600) and out of version control;
composewright keeps an encrypted copy, so `composewright show docker --reveal` prints them again.
