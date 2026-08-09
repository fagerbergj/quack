# Architecture — element vocabulary

## Built-in icons

Available with no setup:

| Icon | Typical use |
|---|---|
| `cloud` | a group boundary, gateway, or managed cloud service |
| `database` | relational/NoSQL database |
| `disk` | storage, cache, volume |
| `internet` | external network / public internet |
| `server` | compute — VM, container host, app server |

Custom icons: register an iconify.design pack, then reference `packname:icon-name`, e.g. `service db(logos:aws-aurora)[Database]`. Unregistered packs render nothing usable outside mermaid's own live editor.

## Groups

```text
group {id}({icon})[{title}] (in {parentId})?
```

Groups nest via `in`. A group is itself referenced in edges only through the `{group}` modifier on a member service (`serviceId{group}:side`), never by its own id.

## Services

```text
service {id}({icon})[{title}] (in {parentId})?
```

## Junctions

```text
junction {id} (in {parentId})?
```

No icon or label — a pure 4-way split point for routing edges that need to branch (e.g. one disk feeding two independent gateways through a shared junction).

## Edge directions

Each edge end names the side of the service/junction it leaves from:

| Symbol | Side |
|---|---|
| `T` | top |
| `B` | bottom |
| `L` | left |
| `R` | right |

Full edge grammar:

```text
{id}{group}?:{T|B|L|R} {<}?--{>}? {T|B|L|R}:{id}{group}?
```

`<` before `--` puts an arrowhead at the left/first endpoint; `>` after `--` puts one at the right/second endpoint. Omit both for an undirected line.

## Align

```text
align row {id} {id} ...
align column {id} {id} ...
```

Pins members to a shared y (`row`) or x (`column`). Requires 2+ already-declared services/junctions; order in the list sets position along the axis. v11.16.0+.
