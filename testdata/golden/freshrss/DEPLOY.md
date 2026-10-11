# freshrss

FreshRSS is a self-hosted RSS feed aggregator.

Upstream: https://github.com/FreshRSS/FreshRSS
Validation: not validated yet

## Start it

In the folder that holds compose.yaml and .env:

    docker compose up -d
    docker compose ps

## Open it

Published on this machine only: http://localhost:8080

## Data

Data lives in named Docker volumes (`docker volume ls | grep freshrss`); `docker compose down -v` deletes them.

## After the first start

- The [default user](13_Default_user.md) is marked with italic letters. (docs/en/admins/12_User_management.md:7)
- # Default user (docs/en/admins/13_Default_user.md:1)
- Currently, we have one main user, also called default user, or admin. All the others are regular users. (docs/en/admins/13_Default_user.md:3)
- The default user is the first created user within the install routine. (docs/en/admins/13_Default_user.md:5)
- The default user is always an administrator. (docs/en/admins/13_Default_user.md:9)
- The default user is used for the anonymous reading. (docs/en/admins/13_Default_user.md:15)
- * enable the checkbox of Allow anonymous reading of the default user’s articles (docs/en/admins/13_Default_user.md:19)
- * when no user is logged in then the feeds of the default user is shown (docs/en/admins/13_Default_user.md:20)
- ## Change the default user (docs/en/admins/13_Default_user.md:22)
- There is no UI for changing the default user, but a CLI (./cli/reconfigure.php --default_user YourNewAdmin) is provided. It can also be changed manually by editing the text file ./FreshRSS/data/config.php and changing 'default_user' => 'alice', to the desired user. (docs/en/admins/13_Default_user.md:24)
- * Enter the exact username that matches your OIDC identity (e.g., admin@idm.example.com) as the default user (docs/en/admins/16_OpenID-Connect.md:22)
- > ⚠️ Important: Using a random username instead of your actual OIDC identity as the default user may result in no administrator access to your instance. (docs/en/admins/16_OpenID-Connect.md:28)
- FreshRSS has a default user agent string that can be overwritten in each feed setting. (docs/en/admins/17_configs_not_ui.md:97)
- This allows anyone to update the feeds of the default user’s subscriptions. (docs/en/users/09_refreshing_feeds.md:87)
- If your FreshRSS instance uses no authentication (public instance, default user): (docs/en/users/09_refreshing_feeds.md:105)

## Secrets

.env holds the generated secrets in plain text. Keep it private (mode 0600) and out of version control;
composewright keeps an encrypted copy, so `composewright show freshrss --reveal` prints them again.
