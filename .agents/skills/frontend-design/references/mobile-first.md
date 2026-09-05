# Mobile-first (quack chat UI)

Principles and defects from [Epic #1131](https://github.com/fagerbergj/quack/issues/1131) (issues #1133-#1138). Read this when touching layout, navigation, dialogs, or any component that must work below 600px.

## Decision rules

Each rule states the source, then quack's own mapping.

1. **Breakpoints = Material 3 window size classes.** Compact 0-599dp (single column, touch-first), medium 600-839dp, expanded 840dp+ (multi-pane). ([m3.material.io](https://m3.material.io/foundations/layout/breakpoints/overview))
   Quack: design at **360px first** (narrowest target device), verify it holds, then add `sm:`/`md:` variants as width grows past 600px. Never start from a desktop layout and shrink it - a layout that "just happens" to fit 390px because of a fixed sidebar is not adapted.

2. **Hidden nav costs completion.** NN/g's 179-participant study of three nav patterns found hidden nav rated 21% harder than visible nav (11% harder than the "combo" pattern) and 15% slower on mobile than combo (39% slower on desktop); hidden nav scored worst of the tested patterns. ([nngroup.com](https://www.nngroup.com/articles/hamburger-menus/))
   Quack: `NavRail` (`frontend/src/components/NavRail.tsx`) is an off-canvas drawer at **every** width - closed it renders nothing, open it floats over the content - with a visible ⊞ toggle (`NavToggle`) in each page's header leading slot as the entry point; per #1171 there is no persistent rail and no hamburger column. **No bottom tab bar** - two top-level destinations (Chats, Memory) plus a chat-list drawer is NN/g's "combo" pattern (visible entry point + menu), which a footer nav doesn't fit.

3. **Touch targets: 24px is the AA floor, 44px is the target.** WCAG 2.2 SC 2.5.8 requires >=24x24 CSS px for any pointer target (padding counts). ([wcag22aa.org](https://wcag22aa.org/new-criteria/target-size/)) Apple HIG's long-standing guidance is 44x44pt for primary actions. ([uxcel.com](https://uxcel.com/lessons/ios-app-design-712))
   Quack: 24px is an absolute floor, never violate it. 44px is the target for anything tapped repeatedly (send, attach, chat-actions, archive, close). Grow icon buttons with padding, not a bigger icon. Adjacent small targets (e.g. archive buttons packed against a chat row) need separation, not just size.

4. **Responsive layout / container queries.** MDN's responsive-design guidance plus Tailwind v4's native `@container` support. ([developer.mozilla.org](https://developer.mozilla.org/en-US/docs/Learn_web_development/Core/CSS_layout/Responsive_Design), [tailwindcss.com](https://tailwindcss.com/docs/responsive-design))
   Quack: wide content (tables, code blocks, mermaid) scrolls horizontally inside its own `overflow-x-auto` container sized to the viewport - it never expands the page or a card past the viewport width. Use `@container` (Tailwind v4: wrap the parent in `@container`, style children with `@sm:`/`@md:` variants) where a card's *own* available width, not the viewport, should drive its layout - e.g. DAG node cards inside a possibly-narrowed content column.

5. **Typography floor.** Body text stays >=16px on mobile - smaller inputs trigger iOS auto-zoom on focus. Truncate headings/labels only with a way to read the rest (tooltip, wrap, or an expanded view) - never truncate to 3-4 characters with no recovery path.

6. **Dialogs become sheets on compact width.** Current mobile convention plus the same MDN responsive-design source above: a modal sized for desktop and centered on a phone leaves most of the screen as dead backdrop.
   Quack: any dialog opened at compact width should be a full-width (or full-screen) sheet - `fixed inset-x-0 bottom-0 w-full max-h-[90vh]` below `sm:`, keeping the existing centered-modal treatment at `sm:` and up - with a full-size header and a >=44px close target.

## Three non-negotiables

- No horizontal scroll on the page itself (wide content scrolls inside its own container).
- No interactive target under 44px for anything tapped repeatedly.
- Off-canvas drawers/sheets on compact width, not narrowed-but-still-persistent chrome.

## Before/after patterns from the audit (#1133-#1138)

- **#1133 - persistent nav rail.** `NavRail` was a fixed `w-40` column on every viewport (44% of a 360px screen), squeezing the composer to 55px wide. The #1133 fix made it an off-canvas drawer below the 600px breakpoint (reusing the existing chat-list drawer's slide-in pattern), with a visible entry point replacing the always-visible rail. **Superseded by #1171:** the persistent rail is gone at every width - the drawer is the app's only navigation shape, opened from the ⊞ toggle in each page's header and rendering nothing when closed. The #1133 work still reduced the severity of #1134 and #1136 downstream.
- **#1134 - DAG node cards don't wrap.** Sibling node cards laid out in a non-wrapping row truncated titles to 1-2 characters at 96px card width. Fix: default to a stacked column (`flex-col`), promote to side-by-side only at `sm:flex-row` and up.
- **#1135 - artifacts dialog isn't sized for compact width.** In `ArtifactPanel.tsx`, the header (`<h2 ... truncate>`) truncates the artifact name with no way to read the rest, and the close/pin buttons (`h-7 w-7`, 28px) sit under the 44px target. Fix: keep the dialog full-viewport on compact width (drop the truncate on the header or let it wrap), and pad those buttons to 44px; audit other dialogs (rename, filters) for the same fixed-desktop-size assumptions while in there.
- **#1136 - chat header title truncates to 4-6 characters.** Token-count pill was given layout priority over the title. Fix: `flex-1 min-w-0 truncate` on the title, and let less-critical metadata (token count) collapse or move behind the "..." menu first.
- **#1137 - icon buttons and the memory scope selector under 44px.** Chat-actions button measured 28x28px; the Memory scope selector rendered as a single unlabeled letter. Fix: pad icon-only buttons to 44px rather than resizing icons; give the scope selector a real label or accessible name instead of a truncated single character.
- **#1138 - attached images render as a text placeholder.** `[User attached: 1 file(s): image/png]` with no thumbnail. Fix: render an `<img>` (`max-w-full`, capped thumbnail size) wherever an attachment URL/blob is available in the turn data, instead of only the text placeholder. Not mobile-specific, but costlier on mobile where re-checking via another device is less convenient.

## Validation loop

1. Emulate **360x740** and **390x844** in both light and dark theme (narrowest target device, then the more common phone size). Chrome DevTools MCP: `mcp__chrome-devtools__resize_page` to each size, `mcp__chrome-devtools__emulate` for color-scheme, `mcp__chrome-devtools__take_screenshot` to inspect.
2. If a screenshot call times out or the tool is unavailable, fall back to `mcp__chrome-devtools__take_snapshot` (accessibility tree) and check computed sizes via `mcp__chrome-devtools__evaluate_script` (`getBoundingClientRect()` on the element in question) - the #1133 measurements above were taken this way.
3. Confirm no horizontal scrollbar on the page (`document.documentElement.scrollWidth <= window.innerWidth`); any wide content should scroll inside its own container instead.
4. Confirm interactive targets meet the 44px target via `getBoundingClientRect()` - not just "looks tappable."
5. Test RTL: under `dir="rtl"`, Tailwind v4 logical utilities (`ms-`/`me-`/`start-`/`end-`) flip automatically but physical ones (`left-`/`right-`/`ml-`/`mr-`/`text-left`) do not - verify RTL rendering of any physical-direction classes, and confirm JS `matchMedia` viewport-width logic isn't direction-dependent.
6. Repeat at each size/theme/direction combination until clean - a fix that only checked one viewport is not verified.
