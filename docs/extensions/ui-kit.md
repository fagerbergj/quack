# Extension UI kit

An SDK extension that implements `sdk.UI` gets a nav entry in the SPA's left rail (`NavRail`), pointing at one of its own routes (`sdk.UIDescriptor.Href`). That route is a full page navigation, outside the SPA's own router - if it serves HTML, that HTML is a separate document with none of the SPA's Tailwind build or component styles available to it.

Quack serves a small, hand-authored CSS file for exactly this: same-origin, no build step, no shared toolchain with the extension's own module.

```
/assets/ext/v1/kit.css
```

The `quack-extensions/sdk` module exports the path as a constant - `sdk.UIKitCSS` - so an extension doesn't hardcode the string.

## Using it

Link it from any HTML an extension's handler writes:

```html
<link rel="stylesheet" href="/assets/ext/v1/kit.css">
```

It defines CSS custom properties for the host's tokens (`--qk-bg`, `--qk-surface`, `--qk-text`, `--qk-muted`, `--qk-accent`, `--qk-border`, `--qk-radius`, fonts) and a small primitive set built from them: `.qk-page`, `.qk-card`, `.qk-chip`, `.qk-badge` (+ `--ok`/`--warn`/`--err`), `.qk-table` (wrap it in `.qk-table-wrap` for horizontal overflow), `.qk-btn` (+ `--primary`). See the file itself (`frontend/public/assets/ext/v1/kit.css`) for the full class list - it's short enough to read directly.

## Versioning

The `v1` in the path is the contract version, not a build hash: it is frozen-additive. Existing tokens and classes keep their current meaning for as long as `/v1/` is live; nothing here gets renamed, removed, or restyled underneath a page already linking it. A breaking visual overhaul ships as `/assets/ext/v2/kit.css` - `/v1/` keeps serving unchanged, so an extension only needs to move to `/v2/` on its own schedule, not quack's.

## What the kit is not

The status colors (`--qk-ok-*`, `--qk-warn-*`, `--qk-err-*`) follow Tailwind's
stock green/amber/red — the same palette the SPA's own badges use. quack's
custom theme only overrides the grays and the blue accent, and the kit mirrors
that split exactly: customize-worthy tokens track the theme, status colors
track Tailwind. If the SPA's badge palette ever diverges, the kit follows the
SPA, additively (see the v1 contract at the top of kit.css).
