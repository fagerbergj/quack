# C4 — examples

## 1. System Context — e-commerce platform

What this shows: the top-level view — who uses the system and which external systems it talks to.

```mermaid
C4Context
title System Context diagram for Shoply E-commerce Platform

Person(customer, "Customer", "Browses products and places orders")
Person(support, "Support Agent", "Helps customers with orders")

System(shoply, "Shoply", "Lets customers browse products, place orders, and track shipments")
System_Ext(payment, "Payment Gateway", "Processes card and wallet payments")
System_Ext(shipping, "Shipping Provider", "Delivers packages to customers")

Rel(customer, shoply, "Browses and orders using")
Rel(support, shoply, "Manages orders via")
Rel(shoply, payment, "Charges cards using", "HTTPS/REST")
Rel(shoply, shipping, "Requests deliveries using", "HTTPS/REST")
```

## 2. Container — inside the e-commerce platform

What this shows: one level deeper — the deployable units inside Shoply and how they talk to each other and to the payment gateway.

```mermaid
C4Container
title Container diagram for Shoply E-commerce Platform

Person(customer, "Customer", "Browses products and places orders")

Container_Boundary(shoply, "Shoply") {
    Container(spa, "Web App", "React, TypeScript", "Product browsing and checkout UI")
    Container(api, "API Application", "Go, chi", "Serves product, cart, and order APIs")
    ContainerDb(db, "Order Database", "PostgreSQL", "Stores orders, products, and customer accounts")
    Container(worker, "Order Worker", "Go", "Processes async order fulfillment jobs")
}

System_Ext(payment, "Payment Gateway", "Processes payments")

Rel(customer, spa, "Uses", "HTTPS")
Rel(spa, api, "Makes API calls to", "JSON/HTTPS")
Rel(api, db, "Reads from and writes to", "SQL")
Rel(api, worker, "Queues jobs for", "async")
Rel(worker, db, "Updates order status in", "SQL")
Rel(api, payment, "Charges via", "HTTPS")
```

## 3. Component — inside the API Application

What this shows: the internal components of a single container, useful when a container is complex enough to need its own breakdown.

```mermaid
C4Component
title Component diagram for Shoply API Application

Container(spa, "Web App", "React", "Checkout UI")
ContainerDb(db, "Order Database", "PostgreSQL", "Order and product data")
System_Ext(payment, "Payment Gateway", "Processes payments")

Container_Boundary(api, "API Application") {
    Component(orders, "Orders Controller", "Go handler", "Creates and reads orders")
    Component(cart, "Cart Controller", "Go handler", "Manages shopping carts")
    Component(paysvc, "Payment Service", "Go package", "Talks to the payment gateway")
    Component(repo, "Order Repository", "Go package", "Persists orders to Postgres")

    Rel(orders, repo, "Uses")
    Rel(orders, paysvc, "Uses")
    Rel(cart, repo, "Uses")
    Rel(paysvc, payment, "Calls", "HTTPS")
    Rel(repo, db, "Reads from and writes to", "SQL")
}

Rel(spa, orders, "Calls", "JSON/HTTPS")
Rel(spa, cart, "Calls", "JSON/HTTPS")
```

## 4. Deployment — production topology

What this shows: where containers actually run — CDN, Kubernetes pods, and a managed database instance.

```mermaid
C4Deployment
title Deployment diagram for Shoply - Production

Deployment_Node(cdn, "CDN", "CloudFront"){
    Container(spa, "Web App", "React static build", "Serves the checkout UI")
}

Deployment_Node(cluster, "Production Cluster", "Kubernetes"){
    Deployment_Node(apipod, "api-* x3", "Pod"){
        Container(api, "API Application", "Go", "Serves product and order APIs")
    }
    Deployment_Node(workerpod, "worker-* x2", "Pod"){
        Container(worker, "Order Worker", "Go", "Processes fulfillment jobs")
    }
}

Deployment_Node(rds, "Managed Postgres", "AWS RDS"){
    ContainerDb(db, "Order Database", "PostgreSQL 16", "Stores orders and products")
}

Rel(spa, api, "Calls", "JSON/HTTPS")
Rel(api, db, "Reads from and writes to", "JDBC")
Rel(worker, db, "Updates", "JDBC")
```
