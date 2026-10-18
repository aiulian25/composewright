# wallabag

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

Data lives in named Docker volumes (`docker volume ls | grep wallabag`); `docker compose down -v` deletes them.

## After the first start

- Default login is wallabag:wallabag. (README.md:13)
- MYSQL_ROOT_PASSWORD was generated for you; it is in .env
- SYMFONY__ENV__DATABASE_PASSWORD was generated for you; it is in .env
- wallabag: upstream publishes container port 80 on any free host port; host port 8080 is used so the address stays the same
- wallabag: upstream bind mount /opt/wallabag/images:/var/www/wallabag/web/assets/images is a named volume here (use --storage bind for a host folder)
- db: upstream bind mount /opt/wallabag/data:/var/lib/mysql is a named volume here (use --storage bind for a host folder)

## Secrets

.env holds the generated secrets in plain text. Keep it private (mode 0600) and out of version control;
composewright keeps an encrypted copy, so `composewright show wallabag --reveal` prints them again.
