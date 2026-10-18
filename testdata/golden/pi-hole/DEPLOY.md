# pihole

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

Data lives in named Docker volumes (`docker volume ls | grep pihole`); `docker compose down -v` deletes them.

## After the first start

- FTLCONF_webserver_api_password was generated for you; it is in .env
- pihole: upstream publishes host port 53; a port above 1024 is used so no extra privilege is needed
- pihole: upstream publishes host port 53; a port above 1024 is used so no extra privilege is needed
- pihole: upstream publishes host port 80; a port above 1024 is used so no extra privilege is needed
- pihole: upstream publishes host port 443; a port above 1024 is used so no extra privilege is needed
- pihole: upstream bind mount ./etc-pihole:/etc/pihole is a named volume here (use --storage bind for a host folder)

## Secrets

.env holds the generated secrets in plain text. Keep it private (mode 0600) and out of version control;
composewright keeps an encrypted copy, so `composewright show pihole --reveal` prints them again.
