# TreeView — examples

## 1. Monorepo layout (indentation)

What this shows: a Go backend + frontend monorepo, the plain indentation form.

```mermaid
treeView-beta
    quack/
        internal/
            orchestrator/
            vetting/
            dag/
        cmd/
            quack/
        agents/
        openapi.yaml
        go.mod
```

## 2. Frontend source tree with annotations

What this shows: `:::highlight` on the entry component and `##` descriptions marking notable files, plus a generated directory that should not be hand-edited.

```mermaid
treeView-beta
├── src/
│   ├── components/
│   │   ├── Chat.tsx :::highlight ## main chat view
│   │   └── Sidebar.tsx
│   ├── state/
│   │   ├── chatStore.ts
│   │   └── agentStream.ts ## SSE parsing
│   └── generated/ ## do not edit
├── package.json
└── vite.config.ts
```

## 3. Repo layout with file-type icons

What this shows: `showIcons` plus `filenameIcons`/`extensionIcons` config maps resolving icons by filename and extension through a registered icon pack.

```mermaid
---
config:
  treeView:
    showIcons: true
    defaultIconPack: material-icon-theme
    filenameIcons:
      Dockerfile: docker
    extensionIcons:
      .ts: typescript
      .tsx: react-ts
      .yaml: yaml
---
treeView-beta
    quack/
        internal/
            orchestrator/
        frontend/
            src/
                App.tsx
        Dockerfile
        openapi.yaml
        README.md
```

## 4. Agent bundles with quoted labels and a comment

What this shows: directory names with spaces (quoted), a comment marking a generated subtree, and multiple sibling bundle directories.

```mermaid
treeView-beta
    "release notes"/
        "v0.14.0.md"
        "v0.13.0.md"
    agents/
        code-implementer/
            agent-card.json
            prompt.md
        web-researcher/
            agent-card.json
            prompt.md
    %% generated client, do not hand-edit
    frontend/
        src/
            generated/
```
