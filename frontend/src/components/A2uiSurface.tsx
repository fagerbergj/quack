import { createContext, createElement, useContext, useId, useLayoutEffect, useState, type KeyboardEvent } from 'react'
import { z } from 'zod'
import { A2uiSurface, basicCatalog, createComponentImplementation, type ReactComponentImplementation } from '@a2ui/react/v0_9'
import { AccessibilityAttributesSchema, ButtonApi, Catalog, ChoicePickerApi, DynamicStringSchema, IconApi, MessageProcessor, TabsApi, TextApi } from '@a2ui/web_core/v0_9'
import { MermaidDiagram } from './MermaidDiagram'
import { DiffView } from './ArtifactPanel'
import { AssistantText } from './AgentParts'
import { Icon as QuackIcon, ICON_NAMES, type IconName } from './Icon'
import { QUACK_CATALOG_ID, surfaceMessages, type A2uiActionRequest, type SurfaceContent } from '../lib/a2ui'

const common = { accessibility: AccessibilityAttributesSchema.optional(), weight: z.number().optional() }

const Mermaid = createComponentImplementation(
  { name: 'Mermaid', schema: z.object({ ...common, code: DynamicStringSchema }).strict() },
  ({ props }) => <MermaidDiagram code={props.code ?? ''} />,
)

const Code = createComponentImplementation(
  {
    name: 'Code',
    schema: z.object({
      ...common,
      path: z.string(),
      startLine: z.number().int().optional(),
      language: z.string().optional(),
      code: DynamicStringSchema,
    }).strict(),
  },
  ({ props }) => (
    <figure className="min-w-0">
      <figcaption className="mb-1 font-mono text-xs text-gray-500 dark:text-gray-400 break-all">
        {props.path}{props.startLine != null ? `:${props.startLine}` : ''}
      </figcaption>
      <DiffView text={props.code ?? ''} />
    </figure>
  ),
)

const HEADING: Record<string, string> = {
  h1: 'text-xl font-semibold', h2: 'text-lg font-semibold', h3: 'text-base font-semibold', h4: 'text-sm font-semibold', h5: 'text-sm font-medium',
}

// True while a turn is being sent or streamed: actions are held off rather than dropped.
const Busy = createContext(false)
// A label inside a <button> stays inline text: no block markdown, no nested links.
const InButton = createContext(false)

// Body text goes through the chat's own markdown pipeline (sanitized, same look as answers).
const Text = createComponentImplementation(TextApi, ({ props }) => {
  const inButton = useContext(InButton)
  const text = props.text ?? ''
  const variant = props.variant ?? 'body'
  if (inButton) return <span>{text}</span>
  if (variant === 'body') return <AssistantText text={text} />
  if (variant === 'caption') return <p className="text-xs text-gray-500 dark:text-gray-400">{text}</p>
  return createElement(variant, { className: `${HEADING[variant]} text-gray-900 dark:text-gray-100` }, text)
})

const BUTTON_VARIANT: Record<string, string> = {
  primary: 'border-transparent bg-blue-600 text-white hover:bg-blue-700',
  default: 'border-gray-300 dark:border-gray-600 bg-white dark:bg-gray-800 text-gray-800 dark:text-gray-100 hover:bg-gray-50 dark:hover:bg-gray-700',
  borderless: 'border-transparent text-blue-600 dark:text-blue-400 hover:underline',
}

const Button = createComponentImplementation(ButtonApi, ({ props, buildChild }) => {
  const busy = useContext(Busy)
  return (
    <div className="flex flex-wrap items-center gap-x-3 gap-y-1">
      <button
        type="button"
        onClick={props.action}
        disabled={busy || props.isValid === false}
        className={`min-h-[44px] rounded-lg border px-4 text-sm font-medium transition-colors disabled:opacity-50 disabled:cursor-not-allowed ${BUTTON_VARIANT[props.variant ?? 'default']}`}
      >
        <InButton.Provider value>{props.child ? buildChild(props.child) : null}</InButton.Provider>
      </button>
      {busy && <span className="text-xs text-gray-500 dark:text-gray-400">Wait for the current reply to finish</span>}
    </div>
  )
})

// Only names quack's own icon set has; anything else (incl. custom svgPath) renders nothing.
const Icon = createComponentImplementation(IconApi, ({ props }) => {
  const name = typeof props.name === 'string' ? props.name.replace(/[A-Z]/g, c => `_${c.toLowerCase()}`) : ''
  return ICON_NAMES.has(name) ? <QuackIcon name={name as IconName} className="w-5 h-5" /> : null
})

// ponytail: chips/filterable render as the plain list; add them when an agent uses them.
// Nested dynamic values (option labels, tab titles) arrive resolved; the inferred types just don't narrow them.
const ChoicePicker = createComponentImplementation(ChoicePickerApi, ({ props }) => {
  const group = useId()
  const values = Array.isArray(props.value) ? props.value : []
  const single = props.variant !== 'multipleSelection'
  const toggle = (v: string) =>
    props.setValue(single ? [v] : values.includes(v) ? values.filter(x => x !== v) : [...values, v])
  return (
    <fieldset className="min-w-0">
      {props.label && <legend className="mb-1.5 text-sm font-medium text-gray-900 dark:text-gray-100">{props.label}</legend>}
      <div className="space-y-0.5">
        {(props.options ?? []).map(o => (
          <label key={o.value} className="flex items-start gap-2 rounded-lg px-2 py-1.5 text-sm text-gray-700 dark:text-gray-200 hover:bg-gray-100 dark:hover:bg-gray-700/50 cursor-pointer">
            <input
              type={single ? 'radio' : 'checkbox'}
              name={single ? group : undefined}
              checked={values.includes(o.value)}
              onChange={() => toggle(o.value)}
              className="mt-0.5 accent-blue-600"
            />
            <span>{o.label as string}</span>
          </label>
        ))}
      </div>
    </fieldset>
  )
})

// Keyed by the component model, which survives revisions and remounts, so a
// graded revision (or the live turn archiving) doesn't bounce the user to the first tab.
const selectedTab = new WeakMap<object, number>()

const TAB_KEYS: Record<string, (i: number, n: number) => number> = {
  ArrowRight: (i, n) => (i + 1) % n, ArrowLeft: (i, n) => (i - 1 + n) % n, Home: () => 0, End: (_, n) => n - 1,
}

const Tabs = createComponentImplementation(TabsApi, ({ props, buildChild, context }) => {
  const id = useId()
  const [picked, setPicked] = useState(() => selectedTab.get(context.componentModel) ?? 0)
  const tabs = props.tabs ?? []
  const sel = Math.max(0, Math.min(picked, tabs.length - 1))
  const select = (i: number) => { selectedTab.set(context.componentModel, i); setPicked(i) }
  const onKeyDown = (e: KeyboardEvent<HTMLDivElement>) => {
    const move = TAB_KEYS[e.key]
    if (!move || tabs.length === 0) return
    e.preventDefault()
    const next = move(sel, tabs.length)
    select(next)
    document.getElementById(`${id}-tab-${next}`)?.focus()
  }
  return (
    <div className="min-w-0">
      <div role="tablist" onKeyDown={onKeyDown} className="mb-3 flex gap-1 overflow-x-auto overflow-y-hidden border-b border-gray-200 dark:border-gray-700">
        {tabs.map((t, i) => (
          <button
            key={i}
            id={`${id}-tab-${i}`}
            type="button"
            role="tab"
            aria-selected={i === sel}
            aria-controls={`${id}-panel`}
            tabIndex={i === sel ? 0 : -1}
            onClick={() => select(i)}
            className={`-mb-px shrink-0 min-h-[44px] border-b-2 px-3 text-sm ${i === sel
              ? 'border-blue-600 font-medium text-blue-600 dark:text-blue-400'
              : 'border-transparent text-gray-500 dark:text-gray-400 hover:text-gray-700 dark:hover:text-gray-200'}`}
          >
            {t.title as string}
          </button>
        ))}
      </div>
      <div role="tabpanel" id={`${id}-panel`} aria-labelledby={`${id}-tab-${sel}`}>{tabs[sel] ? buildChild(tabs[sel].child) : null}</div>
    </div>
  )
})

const overrides: ReactComponentImplementation[] = [Text, Button, Icon, ChoicePicker, Tabs, Mermaid, Code]
const quackCatalog = new Catalog<ReactComponentImplementation>(
  QUACK_CATALOG_ID,
  [...basicCatalog.components.values()].filter(c => !overrides.some(o => o.name === c.name)).concat(overrides),
  [...basicCatalog.functions.values()],
  basicCatalog.themeSchema,
)

interface Entry {
  processor: MessageProcessor<ReactComponentImplementation>
  content?: SurfaceContent
  onAction?: (a: A2uiActionRequest) => void
  error?: string
}

// ponytail: never evicted - one small entry per inline surface seen this session.
const entries = new Map<string, Entry>()

function newEntry(): Entry {
  const entry: Entry = {
    processor: new MessageProcessor([quackCatalog], a => {
      entry.onAction?.({ surface_id: a.surfaceId, name: a.name, source_component_id: a.sourceComponentId, context: a.context })
    }, { version: 'v0.9.1' }),
  }
  return entry
}

// Idempotent per content object; a throw mid-batch leaves the prior render in place with the error shown.
function apply(entry: Entry, content: SurfaceContent): void {
  if (entry.content === content) return
  const local = entry.processor.model.getSurface(content.surface_id)?.dataModel.get('/')
  try {
    entry.processor.processMessages(surfaceMessages(entry.content, content, local))
    entry.error = undefined
  } catch (e) {
    entry.error = e instanceof Error ? e.message : String(e)
  }
  entry.content = content
}

function entryFor(persistKey: string | undefined, content: SurfaceContent): Entry {
  const existing = persistKey ? entries.get(persistKey) : undefined
  if (existing) return existing
  const entry = newEntry()
  apply(entry, content)
  if (persistKey) entries.set(persistKey, entry)
  return entry
}

// Renders one a2ui_surface revision. Same persistKey = same processor across
// remounts and revisions, so a newer revision updates in place and keeps the user's picks.
export default function A2uiSurfaceView({ content, onAction, persistKey, busy = false }: {
  content: SurfaceContent
  onAction?: (a: A2uiActionRequest) => void
  persistKey?: string
  busy?: boolean
}) {
  const [entry] = useState(() => entryFor(persistKey, content))
  const [error, setError] = useState(entry.error)
  useLayoutEffect(() => { entry.onAction = onAction }, [entry, onAction])
  // Updates run here, not in render: they fire signal subscriptions of already-mounted components.
  useLayoutEffect(() => { apply(entry, content); setError(entry.error) }, [entry, content])
  const surface = entry.processor.model.getSurface(content.surface_id)
  return (
    <div className="quack-a2ui min-w-0 text-sm text-gray-900 dark:text-gray-100">
      {error && <p role="alert" className="mb-2 text-xs text-amber-700 dark:text-amber-400">Could not render this surface: {error}</p>}
      {surface?.componentsModel.get('root') && <Busy.Provider value={busy}><A2uiSurface surface={surface} /></Busy.Provider>}
    </div>
  )
}
