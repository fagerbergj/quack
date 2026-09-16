# Event Modeling examples

The three canonical patterns from the Event Modeling cheat sheet — the methodology's own vocabulary for what a timeline segment is doing.

## State Change

What this shows: a UI trigger issues a command, which produces an event — the basic write path.

```mermaid
eventmodeling

tf 01 ui CartUI
tf 02 cmd AddItem
tf 03 evt ItemAdded
```

## State View

What this shows: an event feeds a read model, which a UI then displays — the basic read path.

```mermaid
eventmodeling

tf 03 evt ItemAdded
tf 02 rmo CartItems
tf 04 ui CartUI
```

## Translation (automation across a boundary)

What this shows: an external event is picked up by a processor, which issues a command that produces a new internal event — how one bounded context reacts to another.

```mermaid
eventmodeling

tf 03 evt External.InventoryChanged
tf 02 pcr InventoryProcessor
tf 04 cmd ChangeInventory
tf 05 evt Cart.InventoryChanged
```
