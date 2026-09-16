# ER diagram examples

## E-commerce order schema

What this shows: one-to-many identifying relationships with attributes and primary keys.

```mermaid
erDiagram
    CUSTOMER ||--o{ ORDER : places
    CUSTOMER {
        string id PK
        string name
        string email UK
    }
    ORDER ||--|{ LINE-ITEM : contains
    ORDER {
        string id PK
        string customerId FK
        date placedAt
    }
    LINE-ITEM {
        string productCode
        int quantity
        float pricePerUnit
    }
```

## Blog schema with a many-to-many tag relationship

What this shows: an aliased entity and a non-identifying many-to-many relationship.

```mermaid
erDiagram
    a["Author"] {
        string id PK
        string name
    }
    p["Post"] {
        string id PK
        string authorId FK
        string title
    }
    c["Comment"] {
        string id PK
        string postId FK
        string body
    }
    t["Tag"] {
        string id PK
        string label
    }
    a ||--o{ p : writes
    p ||--o{ c : has
    p }o..o{ t : tagged_with
```

## Car insurance named-driver resolution

What this shows: resolving a many-to-many relationship into an identifying join entity (adapted from the mermaid docs' own example).

```mermaid
erDiagram
    CAR ||--o{ NAMED-DRIVER : allows
    CAR {
        string registrationNumber PK
        string make
        string model
    }
    PERSON ||--o{ NAMED-DRIVER : is
    PERSON {
        string driversLicense PK "The license #"
        string firstName
        string lastName
    }
    NAMED-DRIVER {
        string carRegistrationNumber PK, FK
        string driverLicence PK, FK
    }
```

## User auth schema

What this shows: a role/permission many-to-many join and optional (nullable, v11.16.0+) attributes.

```mermaid
erDiagram
    USER ||--o{ USER-ROLE : has
    ROLE ||--o{ USER-ROLE : grants
    ROLE ||--o{ ROLE-PERMISSION : includes
    PERMISSION ||--o{ ROLE-PERMISSION : grants
    USER {
        string id PK
        string email UK
        string? displayName
    }
    ROLE {
        string id PK
        string name
    }
    PERMISSION {
        string id PK
        string action
    }
```
