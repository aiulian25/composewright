# docker-bookstack

The LinuxServer.io team brings you another container release featuring:

Upstream: https://github.com/linuxserver/docker-bookstack
Validation: not validated yet

## Start it

In the folder that holds compose.yaml and .env:

    docker compose up -d
    docker compose ps

## Open it

Published on this machine only: http://localhost:6875

## Data

Data lives in named Docker volumes (`docker volume ls | grep docker-bookstack`); `docker compose down -v` deletes them.

## After the first start

- The default username is admin@admin.com with the password of password, access the container at http://<host ip>:6875. (README.md:62)
- APP_KEY was generated for you; it is in .env
- DB_PASSWORD was generated for you; it is in .env
- MARIADB_ROOT_PASSWORD was generated for you; it is in .env
- docker-bookstack: DB_HOST was left for a database you run; the stack runs docker.io/library/mariadb:11.8 as mariadb and points DB_HOST at it

## Secrets

.env holds the generated secrets in plain text. Keep it private (mode 0600) and out of version control;
composewright keeps an encrypted copy, so `composewright show docker-bookstack --reveal` prints them again.
