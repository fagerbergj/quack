# Architecture — examples

## 1. Three-tier web app

What this shows: a load balancer fanning out to two app servers that share a database and a cache — a standard scale-out web tier.

```mermaid
architecture-beta
    group vpc(cloud)[Production VPC]

    service internet(internet)[Internet]
    service lb(server)[Load Balancer] in vpc
    service app1(server)[App Server 1] in vpc
    service app2(server)[App Server 2] in vpc
    service db(database)[Primary DB] in vpc
    service cache(disk)[Cache] in vpc

    internet:B --> T:lb
    lb:B --> T:app1
    lb:B --> T:app2
    app1:R --> L:db
    app2:R --> L:db
    app1:B --> T:cache
    app2:B --> T:cache
```

## 2. Multi-source data pipeline (using `align`)

What this shows: three source databases feeding a single ETL service — `align column` keeps them stacked instead of collapsing onto each other, since all three share the same `R --> L` port pair into `etl`.

```mermaid
architecture-beta
    group ingest(cloud)[Ingest]
    service orders_db(database)[Orders DB] in ingest
    service users_db(database)[Users DB] in ingest
    service events_db(database)[Events DB] in ingest
    service etl(server)[ETL Service] in ingest

    orders_db:R --> L:etl
    users_db:R --> L:etl
    events_db:R --> L:etl

    align column orders_db users_db events_db
```

## 3. CI/CD pipeline across stages

What this shows: source, build, and deploy as separate groups, with a straight-line pipeline crossing group boundaries.

```mermaid
architecture-beta
    group source(cloud)[Source]
    group build(cloud)[Build]
    group deploy(cloud)[Deploy]

    service repo(internet)[Git Repo] in source
    service ci(server)[CI Runner] in build
    service registry(disk)[Image Registry] in build
    service prod(server)[Production] in deploy

    repo:R --> L:ci
    ci:R --> L:registry
    registry:R --> L:prod
```

## 4. Storage fan-in with a junction

What this shows: two disks and a database routed through a shared junction before reaching a backup server — useful when several nodes need to merge onto one path without pairwise edges.

```mermaid
architecture-beta
    service disk1(disk)[Volume 1]
    service disk2(disk)[Volume 2]
    service db(database)[Database]
    service backup(server)[Backup Server]
    junction j1

    disk1:R -- L:j1
    disk2:R -- L:j1
    db:B -- T:j1
    j1:R -- L:backup
```
