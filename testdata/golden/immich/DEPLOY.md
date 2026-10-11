# immich-server

Access the demo here. For the mobile app, you can use https://demo.immich.app for the Server Endpoint URL.

Upstream: https://github.com/immich-app/immich
Validation: not validated yet

## Start it

In the folder that holds compose.yaml and .env:

    docker compose up -d
    docker compose ps

## Open it

Published on this machine only: http://localhost:2283

## Data

Data lives in named Docker volumes (`docker volume ls | grep immich-server`); `docker compose down -v` deletes them.

## After the first start

- Open [localhost:8888](http://localhost:8888) and login with the default credentials from above. (docs/docs/guides/database-gui.md:35)
- - The default admin user must be enabled to run containers. (docs/docs/install/qnap.md:17)
- Since TrueNAS Community Edition 24.10.2.2 and later, Immich can be run as any user and group, the default user being apps (UID 568) and the default group being apps (GID 568). This user, either apps or another user you choose, must have _modify_ permissions on the data dataset. (docs/docs/install/truenas.md:51)
- Application in TrueNAS runs as a specific user and group. Immich uses the default user apps (UID 568) and the default group apps (GID 568). (docs/docs/install/truenas.md:171)
- 2. Update the datasets permissions: Ensure that the datasets used for Immich data storage (library, upload, thumbs, profile, video, backups) have the correct permissions set for the user who will run Immich. The user should have _modify_ permissions on these datasets. The default user for Immich is apps (UID 568) and the default group is apps (GID 568). If you are using a different user, make sure to set the permissions accordingly. You can do this from the TrueNAS web interface by going to the Datasets screen, selecting each dataset, clicking on the Edit button next to Permissions, and adding the user with _modify_ permissions. (docs/docs/install/truenas.md:463)
- DB_PASSWORD was generated for you; it is in .env
- immich-server: upstream bind mount ./library:/data is a named volume here (use --storage bind for a host folder)
- immich-server: upstream mounts the host path /etc/localtime; it is left out
- database: upstream bind mount ./postgres:/var/lib/postgresql/data is a named volume here (use --storage bind for a host folder)

## Secrets

.env holds the generated secrets in plain text. Keep it private (mode 0600) and out of version control;
composewright keeps an encrypted copy, so `composewright show immich-server --reveal` prints them again.
