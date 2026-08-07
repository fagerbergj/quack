# Frontend recipes (concrete patterns)

Load this when **implementing** a change.
Each recipe names the real files in this repo so you can follow the established shape rather than invent one.

---

## 1. Re-render isolation (the typing-lag fix)

Symptom: typing in the chat box lags, especially in long conversations.
Cause: the page component holds the draft `input` state, so every keystroke re-renders the whole turn list (re-parsing markdown / re-rendering DAG trees).
Fix is structural, not virtualization.

**a.
Move draft state out of the page → `components/Composer.tsx`.** The composer owns `input` + `attachments` (+ the file-input ref) locally and hands the finished message up via `onSubmit(text, files, previews)`.
The page no longer re-renders on keystrokes.

```tsx
export function Composer({ disabled, streaming, onSubmit, onStop }: ComposerProps) {
  const [input, setInput] = useState('')
  const [attachments, setAttachments] = useState<AttachmentItem[]>([])
  // … textarea value={input}; on submit: onSubmit(input.trim(), files, previews) then clear.
}
```

**b.
Memoize each completed turn → `components/TurnView.tsx`.** Wrap the per-turn render in `React.memo`.
Completed turns are immutable, so they stop re-parsing markdown/DAG when a *later* turn streams.
Pass `isCopied` (boolean), not the `copied` state.

```tsx
export const TurnView = memo(function TurnView({ turn, idx, choiceAnswer, isChoiceAnswer,
  submittingChoice, isCopied, onChoice, onCopy, onDownload }: TurnViewProps) { /* … */ })
```

**c.
Build sibling-derived props in a `useMemo` keyed to a streaming-stable ref.** Props that depend on neighbours (a turn's clarification answer / whether it *is* a choice answer) must not recompute per token. `state.turns` keeps a stable ref while only `live` changes during streaming (store handlers spread `{ ...s, live }`), so:

```tsx
const turnViews = useMemo(() => state.turns.map((turn, idx, arr) => {
  const turnChoice = pendingChoice(activityFromTurn(turn))
  const next = arr[idx + 1]
  const choiceAnswer = turnChoice ? (next ? next.input.content : live?.userText) : undefined
  const prev = arr[idx - 1]
  const isChoiceAnswer = prev ? pendingChoice(activityFromTurn(prev)) != null : false
  return { turn, idx, choiceAnswer, isChoiceAnswer }
}, [state.turns, live?.userText])   // NOT a per-token dependency
```

**d. `useCallback` every handler** passed to a memoized child (`handleCopy`, `handleDownload`, `handleChoice`, `handleStop`, the submit callback) so identity is stable.

Net effect: the live (streaming) turn re-renders per token; everything else is frozen.
This is why `react-virtuoso` stays deferred - node count is rarely the real bottleneck.

---

## 2. Optimistic-first write in `chatStore.submit` (instant loading indicator)

On a follow-up message, the previous finished turn lingers in `live` and is archived via a GET before the new turn starts.
Writing the new live turn only *after* that GET delays the spinner; clobbering `live` *before* the GET makes the previous answer blink out.
Resolve both with a `submitting`/`pendingUserText` flag written first, keeping the old `live` rendered until `turns` repopulates:

```ts
const prevLive = cur.live
if (cur.live) {
  this.write(chatId, { ...cur, submitting: true, pendingUserText: trimmed, error: '' }) // instant
  try {
    const res = await fetch(`/api/v1/chats/${chatId}`)              // archive (can lag)
    if (res.ok) cur = { ...this.get(chatId), turns: (await res.json()).turns ?? cur.turns }
    else cur = this.get(chatId)
  } catch { cur = this.get(chatId) }
}
const live = { id: '', userText: trimmed, streaming: true, error: '', text: '', runs: [] }
this.write(chatId, { ...cur, live, submitting: false, pendingUserText: undefined, error: '' })
```

The page renders a pending bubble + spinner from `submitting && pendingUserText`; the old `live` (the previous answer) stays visible beneath it until the archive returns.
Regression test in `state/chatStore.test.ts`: with a *hung* archive fetch, assert `submitting === true` synchronously after calling `submit` (before resolving the fetch).

---

## 3. Global theme via Tailwind v4 `@theme` (`frontend/src/index.css`)

Override the built-in scale once; every existing `bg-gray-…`/`border-…`/`bg-blue-…` shifts:

```css
@theme {
  --color-gray-50:  #f7f7f6;   /* … through 950 - soften, don't invert extremes */
  --color-blue-600: #4a5bd4;   /* muted accent */
}
```

Gotcha: **never write `*/` inside the comment** (e.g. `bg-gray-*/border-*`) - it closes the comment early and leaks stray CSS (build warning).
Spell globs out in prose.

**Rules when picking the scale values:**
- Dark page background: tinted near-black (e.g. `#131312`), never `#000` - pure black is eye-searing and makes accents vibrate. Dark surfaces lift via *lightness*, not shadow.
- Dark-mode accents: desaturate ~10–20% vs light; primary text is off-white (e.g. `#f5f5f5`), not `#fff`.
- Don't migrate to semantic tokens (`--color-bg-default`…) just to theme an existing app - that's a full component rewrite. The scale override is the lazy lever. Semantic tokens earn their keep only greenfield or once components need to diverge from the scale.

**Verify WCAG AA after any `@theme` change** - muted greys silently fail.
Run this on the real pairings (4.5:1 normal text, 3:1 large/UI; decorative metadata ~3:1 is acceptable):

```python
def lin(c):
    c/=255
    return c/12.92 if c<=0.03928 else ((c+0.055)/1.055)**2.4
def L(hx):
    hx=hx.lstrip('#'); r,g,b=(int(hx[i:i+2],16) for i in (0,2,4))
    return 0.2126*lin(r)+0.7152*lin(g)+0.0722*lin(b)
def ratio(a,b):
    la,lb=L(a),L(b); hi,lo=max(la,lb),min(la,lb); return (hi+0.05)/(lo+0.05)
# check e.g. ratio('#6b6b63', '#f7f7f6') for secondary text on the page bg → want ≥4.5
```

A warm/light `gray-500` on a light page bg can land ~4.1:1 (under AA) - nudge the shade darker until it clears, keeping the hue.
Verified values for the current warm scale: `gray-500 #6b6b63` (≥4.5:1), `gray-400 #909089` (≥3:1, decorative).
Status colours are never the only cue - pair with icon/label/motion.

---

## 4. Markdown highlight + copy (`components/AgentParts.tsx`)

Add `rehype-highlight` **last** in the plugin array (after sanitize) + a `pre` override:

```tsx
import rehypeHighlight from 'rehype-highlight'
import 'highlight.js/styles/github-dark.css'

<ReactMarkdown
  remarkPlugins={[remarkGfm]}
  rehypePlugins={[rehypeRaw, [rehypeSanitize, mdSchema], rehypeHighlight]}  // highlight LAST
  components={{ pre: CopyablePre }}
>{text}</ReactMarkdown>
```

`CopyablePre` is a `<pre>` wrapper with a hover Copy button that reads `ref.current?.textContent` and calls `navigator.clipboard.writeText`.
Don't fight the sanitizer - order the plugins so its `hljs` classes survive (the `language-*` class survives sanitize either way).

---

## 5. Native-first inputs & accessibility

- **Auto-grow textarea:** `field-sizing-content` + `max-h-48 overflow-y-auto`, drop the fixed-rows reliance. No JS hook. (Chromium-only; degrades gracefully.)
- **Sidebar search (`components/ChatList.tsx`):** `useState` query + `chats.filter(c => (c.title ?? '').toLowerCase().includes(q))` over already-loaded data.
- **a11y:** `role="log" aria-live="polite" aria-atomic="false"` on the streamed answer region; `role="status"` + `aria-label` on spinner bubbles; `aria-label` on icon buttons (📎 ✕ × ☰).

---

## 6. Stories & tests

- Co-locate `*.stories.tsx` for newly extracted/changed surfaces (`Composer`, `ChatList`, `TurnView`, a `WithCodeBlock` story on `AgentParts`). Use the existing CSF format: `import type { Meta, StoryObj } from '@storybook/react-vite'`, `title: 'Chat/<Name>'`.
- Tests are **node-env, logic-only** (`vitest run`); there is no `@testing-library/react`. Test the store/reducers (`state/chatStore.test.ts`, `components/*.test.ts`). Reuse `turnWith(...)` / `makeStream(...)` helpers there. Don't add jsdom + a render library for trivial UI.
