# TreeView

- **Keyword(s):** `treeView-beta`
- **Introduced:** mermaid v11.14.0+. **Beta** — new diagram type, keyword carries `-beta`. GitHub and other renderers pinned to an older mermaid will render nothing for this block.
- **Use when:** showing a directory/file structure — a repo layout, a package tree, a filesystem snapshot.
- **Avoid when:** the hierarchy isn't file-shaped (no natural "directory" vs "leaf" distinction) — use `mindmap` for a general outline, or `flowchart` if nodes need non-tree links.

## Minimal example

```mermaid
treeView-beta
    my-project/
        src/
            index.js
        package.json
```

## Core syntax

Structure is indentation-based, same rule as `mindmap`. A trailing `/` on a label marks it as a directory (renders bold). Labels can be bare or `"quoted"` (quoting is required if the label has spaces).

### Box-drawing input (alternative to indentation)

Paste `tree`/`ls`-style output directly — the parser auto-detects standard (`├──`, `└──`, `│`) or heavy (`┣━━`, `┗━━`, `┃`) Unicode box-drawing characters, no extra config needed:

```mermaid
treeView-beta
├── src/
│   ├── index.ts
│   └── utils.ts
└── package.json
```

### Annotations (combinable, any order)

| Annotation | Syntax | Effect |
|---|---|---|
| Class | `label :::className` | apply a CSS class; built-in `highlight` provided |
| Icon override | `label icon(name)` | explicit per-node icon, always renders even if `showIcons` is off |
| Description | `label ## text` | italic inline description next to the label |

```text
App.tsx :::highlight icon(logos:react) ## main component
```

### Icons

Hidden by default. `showIcons: true` (config) shows built-in `file`/`folder` icons. `filenameIcons` and `extensionIcons` config maps assign icons by exact filename or extension, resolved through a registered iconify pack (`defaultIconPack` for unprefixed names, or `pack:name` explicitly). `.ext: none` / `icon(none)` hides a specific node's icon.

### Comments

`%%` for a whole-line comment, standard mermaid convention.

## Gotchas

- Icon packs are never bundled — an icon reference to an unregistered pack renders as a question mark, not nothing.
- `showIcons` only controls the *default* file/folder icons; an explicit `icon(...)` on a node always renders regardless of that setting.
- Tab characters in indentation-mode input are auto-expanded to spaces, but mixing tabs and spaces inconsistently is still a common source of misparsed depth — parse-error line numbers refer to your original input, which helps when debugging.
- This is one of the newest diagram types in mermaid (v11.14.0) — GitHub's renderer lags upstream mermaid releases and may not render `treeView-beta` blocks at all yet.

## Deeper

- [examples.md](../../assets/treeView-beta/examples.md) — repo layouts, icons, box-drawing input
