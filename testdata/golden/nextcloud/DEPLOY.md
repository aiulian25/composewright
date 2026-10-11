# docker

A safe home for all your data. Access & share your files, calendars, contacts, mail & more from any device, on your terms.

Upstream: https://github.com/nextcloud/docker
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

- The default user within a container is root (uid = 0). By default, processes inside the container will expect to have root privileges. Network services will drop privileges and use www-data to serve requests. (README.md:186)
- Depending on your volumes configuration, this can lead to permission issues. You can address this by running the container with a different default user. When changing the default user, the image will no longer assume it has root privileges and will run all processes under the specified uid. To accomplish this, use the --user / user option in your container environment. (README.md:188)
- POSTGRES_PASSWORD was generated for you; it is in .env
- db: upstream mounts db at /var/lib/postgresql/data; this version of the image keeps its data in /var/lib/postgresql, so it is mounted there

## Secrets

.env holds the generated secrets in plain text. Keep it private (mode 0600) and out of version control;
composewright keeps an encrypted copy, so `composewright show docker --reveal` prints them again.
