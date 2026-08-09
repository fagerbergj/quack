# Class diagram examples

## E-commerce domain model

What this shows: composition/aggregation contrast and cardinality on associations.

```mermaid
classDiagram
    class Order {
        +String id
        +Date placedAt
        +total() BigDecimal
    }
    class LineItem {
        +int quantity
        +BigDecimal unitPrice
    }
    class Customer {
        +String name
        +String email
    }
    Order "1" *-- "1..*" LineItem : contains
    Customer "1" --> "*" Order : places
```

## Plugin interface with implementations

What this shows: an interface annotation, realization arrows, and an abstract base class.

```mermaid
classDiagram
    class Plugin {
        <<interface>>
        +execute(context) bool
    }
    class BasePlugin {
        <<abstract>>
        #String name
        +execute(context)* bool
    }
    class LoggingPlugin {
        +execute(context) bool
    }
    class MetricsPlugin {
        +execute(context) bool
    }
    Plugin <|.. BasePlugin
    BasePlugin <|-- LoggingPlugin
    BasePlugin <|-- MetricsPlugin
```

## Vehicle inheritance hierarchy

What this shows: plain inheritance plus a generic-typed field.

```mermaid
classDiagram
    class Vehicle {
        +int wheels
        +start()
    }
    class Car {
        +int doors
    }
    class Fleet {
        +List~Vehicle~ vehicles
        +addVehicle(Vehicle v)
    }
    Vehicle <|-- Car
    Fleet --> "*" Vehicle : manages
```

## Microservice namespaces

What this shows: grouping related classes into namespaces with cross-namespace relationships.

```mermaid
classDiagram
    namespace Auth {
        class UserService {
            +login()
            +logout()
        }
    }
    namespace Data {
        class Repository {
            +find()
            +save()
        }
    }
    class Gateway {
        +route()
    }
    Gateway --> UserService : delegates
    Gateway --> Repository : delegates
```
