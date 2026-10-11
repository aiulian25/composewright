# mealie

A Place For All Your Recipes

Upstream: https://github.com/mealie-recipes/mealie
Validation: not validated yet

## Start it

In the folder that holds compose.yaml and .env:

    docker compose up -d
    docker compose ps

## Open it

Published on this machine only: http://localhost:9091

## Data

Data lives in named Docker volumes (`docker volume ls | grep mealie`); `docker compose down -v` deletes them.

## After the first start

- !!! warning "Default Username" (docs/docs/documentation/getting-started/installation/installation-checklist.md:80)
- Note that the default username (below) has been changed from previous versions (docs/docs/documentation/getting-started/installation/installation-checklist.md:82)
- !!! tip "Default Credentials" (docs/docs/documentation/getting-started/installation/installation-checklist.md:84)
- After the startup is complete, you should see a login screen. Use the default credentials above to log in and navigate to /admin/site-settings. Here, you'll find a summary of your configuration details and their respective status. Before proceeding, you should validate that the configuration is correct. For any warnings or errors the page will display an error and notify you of what you need to verify. (docs/docs/documentation/getting-started/installation/installation-checklist.md:92)
- POSTGRES_PASSWORD was generated for you; it is in .env

## Secrets

.env holds the generated secrets in plain text. Keep it private (mode 0600) and out of version control;
composewright keeps an encrypted copy, so `composewright show mealie --reveal` prints them again.
