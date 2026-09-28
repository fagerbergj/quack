import { useEffect, useState, useCallback, Fragment } from 'react'
import { api, type Plugin, type PluginUpdate, type PluginReloadReport } from '../api'
import { NavToggle } from '../components/NavToggle'
import { Icon } from '../components/Icon'
import { relativeTime } from '../lib/relativeTime'

export interface PluginsProps {
  navOpen: boolean
  onToggleNav: () => void
  // Storybook/test seam (same pattern as MemoryTab's initialState): pre-seeds
  // state and skips the live fetches, so a story can show empty/populated/
  // error/behind states deterministically with no backend.
  initialPlugins?: Plugin[]
  initialUpdates?: PluginUpdate[]
  // Story-only seam for the action-error banner (severe#3) - never set by
  // App.tsx; a real one only ever comes from a failed Update/Remove.
  initialActionError?: string
  initialReload?: ReloadResult
}

// aborted: the reload endpoint's 422 - nothing swapped, the old generation keeps serving.
interface ReloadResult { reload: PluginReloadReport; aborted?: boolean }

// reloadOf pulls the report off any plugin response or thrown 422 error.
function reloadOf(x: unknown): ReloadResult | null {
  const r = x as Partial<ReloadResult> | null | undefined
  return r?.reload ? { reload: r.reload, aborted: r.aborted } : null
}

function shortSha(sha?: string): string {
  return sha ? sha.slice(0, 7) : '—'
}

// Only a github row tracks a branch; local/embedded rows never had one, so
// the no-ref fallback ("default branch") would otherwise mislabel them.
function refLabel(p: Plugin): string | undefined {
  if (p.source === 'github') return p.ref ?? 'default branch'
  if (p.source === 'embedded') return 'bundled with quack'
  return undefined
}

// useBusyRunner shares the mark-busy/clear-error/run/catch/unmark-busy shape
// every per-row (and update-all) action follows, so handleRemove/handleUpdate/
// handleUpdateAll are each one line instead of a repeated try/catch/finally.
function useBusyRunner(setBusy: React.Dispatch<React.SetStateAction<Set<string>>>, setActionError: (e: string | null) => void, settle: (x: unknown, ok: boolean) => void) {
  return useCallback((key: string, action: () => Promise<unknown>, fallbackMsg: string) => {
    setBusy(prev => new Set(prev).add(key))
    setActionError(null)
    action()
      .then(res => settle(res, true))
      .catch((err: unknown) => {
        setActionError(err instanceof Error ? err.message : fallbackMsg)
        settle(err, false)
      })
      .finally(() => setBusy(prev => { const next = new Set(prev); next.delete(key); return next }))
  }, [setBusy, setActionError, settle])
}

// The dynamic plugin registry page (epic #1427 P2): list, add, remove, and
// update quack's skill plugins. Routed at /plugins - see App.tsx/router.ts.
export default function Plugins({ navOpen, onToggleNav, initialPlugins, initialUpdates, initialActionError, initialReload }: PluginsProps) {
  const [plugins, setPlugins] = useState<Plugin[]>(initialPlugins ?? [])
  const [updates, setUpdates] = useState<Map<string, PluginUpdate>>(
    new Map((initialUpdates ?? []).map(u => [u.name, u])),
  )
  const [loading, setLoading] = useState(initialPlugins === undefined)
  const [loadError, setLoadError] = useState<string | null>(null)
  // actionError is a per-row Update/Remove/Update-all failure - separate from
  // loadError so it never gates the list render (severe#3): the rows that
  // ARE loaded stay visible, with the failure as a banner above them.
  const [actionError, setActionError] = useState<string | null>(initialActionError ?? null)
  const [busy, setBusy] = useState<Set<string>>(new Set())
  const [report, setReport] = useState<ReloadResult | null>(initialReload ?? null)

  // silent: a post-action refresh - keeps the current list on screen instead
  // of flashing back to the loading state.
  const load = useCallback((opts?: { silent?: boolean }) => {
    if (initialPlugins !== undefined) return undefined // story/test seam
    let cancelled = false
    if (!opts?.silent) setLoading(true)
    api.listPlugins()
      .then(list => {
        if (cancelled) return
        setPlugins(list.plugins)
        setLoadError(null)
        // Independent of the list fetch above: an updates-check failure
        // shouldn't blank out the list that already loaded fine.
        api.listPluginUpdates()
          .then(u => { if (!cancelled) setUpdates(new Map(u.updates.map(row => [row.name, row]))) })
          .catch(() => {})
      })
      .catch(e => {
        if (cancelled) return
        const msg = e instanceof Error ? e.message : 'Failed to load plugins'
        // A silent (post-action) refresh failing must not blank the list
        // that's already on screen - surface it as an action error instead.
        if (opts?.silent) setActionError(msg)
        else setLoadError(msg)
      })
      .finally(() => { if (!cancelled && !opts?.silent) setLoading(false) })
    return () => { cancelled = true }
  }, [initialPlugins])

  useEffect(() => load(), [load])

  // A failed action only changed the rows when it still carried a report (a 422 keeps the fetched row).
  const settle = useCallback((x: unknown, ok: boolean) => {
    const r = reloadOf(x)
    setReport(r)
    if (ok || r) load({ silent: true })
  }, [load])
  const runBusy = useBusyRunner(setBusy, setActionError, settle)

  function handleRemove(name: string) {
    if (!window.confirm(`Remove plugin "${name}"? This deletes its clone on disk.`)) return
    runBusy(name, () => api.deletePlugin(name), `Failed to remove ${name}`)
  }

  const handleUpdate = (name: string) =>
    runBusy(name, () => api.updatePlugin(name), `Failed to update ${name}`)

  const handleUpdateAll = () =>
    runBusy('*', () => api.updateAllPlugins(), 'Failed to update plugins')

  const handleReload = () =>
    runBusy('reload', () => api.reloadPlugins(), 'Failed to reload plugins')

  const anyBehind = [...updates.values()].some(u => u.behind)

  return (
    <div className="flex flex-col h-full bg-gray-50 dark:bg-gray-900 text-gray-900 dark:text-white">
      <div className="flex items-center gap-3 px-4 py-3 sm:px-6 border-b border-gray-200 dark:border-gray-700 bg-white dark:bg-gray-800">
        <NavToggle open={navOpen} onToggle={onToggleNav} />
        <h1 className="text-base font-semibold text-gray-900 dark:text-white flex-1">Plugins</h1>
        <button
          onClick={handleReload}
          disabled={busy.has('reload')}
          title="Rebuild agents, workflows and MCP servers from the registered plugins"
          className="flex items-center justify-center gap-1.5 rounded-md px-3 py-1.5 min-h-[44px] text-sm font-medium text-blue-700 dark:text-blue-400 bg-blue-50 dark:bg-blue-900/30 hover:bg-blue-100 dark:hover:bg-blue-900/50 disabled:opacity-40 disabled:cursor-not-allowed transition-colors"
        >
          <Icon name="restart_alt" className="w-4 h-4" />
          Reload
        </button>
        <button
          onClick={handleUpdateAll}
          disabled={!anyBehind || busy.has('*')}
          className="flex items-center justify-center gap-1.5 rounded-md px-3 py-1.5 min-h-[44px] text-sm font-medium text-blue-700 dark:text-blue-400 bg-blue-50 dark:bg-blue-900/30 hover:bg-blue-100 dark:hover:bg-blue-900/50 disabled:opacity-40 disabled:cursor-not-allowed transition-colors"
        >
          <Icon name="refresh" className="w-4 h-4" />
          Update all
        </button>
      </div>

      <AddPluginForm onSettled={settle} />

      {actionError && <ErrorBanner message={actionError} />}

      <div aria-live="polite">
        {report && <ReloadReportPanel result={report} onDismiss={() => setReport(null)} />}
      </div>

      <div className="flex-1 overflow-y-auto overscroll-contain">
        <PluginsBody
          loading={loading}
          loadError={loadError}
          plugins={plugins}
          updates={updates}
          busy={busy}
          onUpdate={handleUpdate}
          onRemove={handleRemove}
        />
      </div>
    </div>
  )
}

function ErrorBanner({ message }: { message: string }) {
  return (
    <div className="m-3 rounded-md bg-red-50 dark:bg-red-950/30 border border-red-200 dark:border-red-800 px-4 py-3 text-sm text-red-700 dark:text-red-400 flex items-center gap-1.5">
      <Icon name="warning" className="w-4 h-4 shrink-0" />
      <span>{message}</span>
    </div>
  )
}

// AddPluginForm owns the entry input's own state/submit/error - kept out of
// Plugins itself so the parent's branch count stays low.
function AddPluginForm({ onSettled }: { onSettled: (x: unknown, ok: boolean) => void }) {
  const [entry, setEntry] = useState('')
  const [addError, setAddError] = useState<string | null>(null)
  const [adding, setAdding] = useState(false)

  async function handleAdd(e: React.FormEvent) {
    e.preventDefault()
    if (!entry.trim()) return
    setAdding(true)
    setAddError(null)
    try {
      const row = await api.createPlugin(entry.trim())
      setEntry('')
      onSettled(row, true)
    } catch (err) {
      setAddError(err instanceof Error ? err.message : 'Failed to add plugin')
      onSettled(err, false)
    } finally {
      setAdding(false)
    }
  }

  return (
    <form onSubmit={e => void handleAdd(e)} className="flex flex-col gap-1.5 px-4 py-3 sm:px-6 border-b border-gray-200 dark:border-gray-700 bg-white dark:bg-gray-800">
      <div className="flex items-center gap-2">
        <input
          type="text"
          value={entry}
          onChange={e => setEntry(e.target.value)}
          placeholder="github:owner/repo[@ref][#path]"
          aria-label="Plugin entry"
          className="flex-1 min-w-0 rounded-md border border-gray-300 dark:border-gray-600 bg-white dark:bg-gray-900 px-3 py-1.5 text-sm placeholder:text-gray-400 dark:placeholder:text-gray-500 focus:outline-none focus:ring-2 focus:ring-blue-500"
        />
        <button
          type="submit"
          disabled={adding || !entry.trim()}
          className="flex items-center justify-center rounded-md px-3 py-1.5 min-h-[44px] medium:min-h-0 text-sm font-medium text-white bg-blue-600 hover:bg-blue-700 disabled:opacity-40 disabled:cursor-not-allowed transition-colors"
        >
          Add
        </button>
      </div>
      {addError && <ErrorBanner message={addError} />}
    </form>
  )
}

function failureSource(f: PluginReloadReport['failures'][number]): string {
  return [f.plugin, f.member].filter(Boolean).join('/') || 'reload'
}

// ReloadReportPanel lists only the non-empty change groups; failures are dropped members, not a failed reload.
function ReloadReportPanel({ result: { reload: r, aborted }, onDismiss }: { result: ReloadResult; onDismiss: () => void }) {
  const groups = ([
    ['Agents added', r.agents.added],
    ['Agents updated', r.agents.updated.map(u => `${u.name} (${u.bundle_hash.slice(0, 7)})`)],
    ['Agents removed', r.agents.removed],
    ['Workflows added', r.workflows.added],
    ['Workflows updated', r.workflows.updated],
    ['Workflows removed', r.workflows.removed],
    ['MCP servers started', r.mcp_servers.started],
    ['MCP servers reused', r.mcp_servers.reused],
    ['MCP servers stopped', r.mcp_servers.stopped],
  ] as const).filter(([, names]) => names.length > 0)
  const tone = aborted
    ? 'bg-amber-50 dark:bg-amber-950/30 border-amber-200 dark:border-amber-800'
    : 'bg-white dark:bg-gray-800 border-gray-200 dark:border-gray-700'

  return (
    <section aria-label="Reload report" className={`m-3 rounded-md border px-4 py-2 text-sm ${tone}`}>
      <div className="flex items-center gap-2">
        <h2 className="flex-1 font-medium">
          {aborted ? `Reload aborted - still serving generation ${r.generation}` : `Reloaded - generation ${r.generation}`}
        </h2>
        <button
          onClick={onDismiss}
          aria-label="Dismiss reload report"
          className="flex items-center justify-center w-11 h-11 -mr-3 rounded-lg text-gray-500 dark:text-gray-400 hover:bg-gray-100 dark:hover:bg-gray-700 transition-colors"
        >
          <Icon name="close" className="w-4 h-4" />
        </button>
      </div>
      {groups.length === 0 && r.failures.length === 0 && (
        <p className="pb-1 text-xs text-gray-500 dark:text-gray-400">No changes</p>
      )}
      {groups.length > 0 && (
        <dl className="grid grid-cols-[auto_1fr] gap-x-3 gap-y-0.5 pb-1 text-xs">
          {groups.map(([label, names]) => (
            <Fragment key={label}>
              <dt className="text-gray-500 dark:text-gray-400">{label}</dt>
              <dd className="min-w-0 break-words">{names.join(', ')}</dd>
            </Fragment>
          ))}
        </dl>
      )}
      {r.failures.length > 0 && (
        <ul className="flex flex-col gap-1 pb-1 text-xs text-amber-700 dark:text-amber-400">
          {r.failures.map((f, i) => (
            <li key={i} className="flex items-start gap-1.5">
              <Icon name="warning" className="w-3.5 h-3.5 shrink-0 mt-0.5" />
              <span className="min-w-0 break-words whitespace-pre-line">
                <span className="font-medium">{failureSource(f)}</span> ({f.stage}): <span className="font-mono">{f.error}</span>
              </span>
            </li>
          ))}
        </ul>
      )}
    </section>
  )
}

// PluginsBody: loading / error / empty / populated, each an exclusive branch
// - split out of Plugins so its own complexity is counted separately.
function PluginsBody({ loading, loadError, plugins, updates, busy, onUpdate, onRemove }: {
  loading: boolean
  loadError: string | null
  plugins: Plugin[]
  updates: Map<string, PluginUpdate>
  busy: Set<string>
  onUpdate: (name: string) => void
  onRemove: (name: string) => void
}) {
  if (loading) {
    return <div className="text-center text-gray-500 dark:text-gray-400 text-sm py-10">Loading…</div>
  }
  if (loadError) {
    return <div className="m-3"><ErrorBanner message={loadError} /></div>
  }
  if (plugins.length === 0) {
    return <div className="text-center text-gray-500 dark:text-gray-400 text-sm py-10">No plugins registered</div>
  }
  return (
    <ul className="divide-y divide-gray-200 dark:divide-gray-700">
      {plugins.map(p => (
        <PluginRow
          key={p.name}
          plugin={p}
          update={updates.get(p.name)}
          busy={busy.has(p.name)}
          onUpdate={() => onUpdate(p.name)}
          onRemove={() => onRemove(p.name)}
        />
      ))}
    </ul>
  )
}

function PluginRow({ plugin: p, update, busy, onUpdate, onRemove }: {
  plugin: Plugin
  update?: PluginUpdate
  busy: boolean
  onUpdate: () => void
  onRemove: () => void
}) {
  // Only a github row is REST-managed - local rows are config (plugins.seed)
  // and re-seeded at boot, so "remove" would just come back; embedded never
  // had a row to remove.
  const removable = p.source === 'github'
  const behind = update?.behind ?? false
  const fetched = relativeTime(p.fetched_at)
  const ref = refLabel(p)

  return (
    <li className="flex items-center gap-3 px-4 py-3 sm:px-6">
      <div className="min-w-0 flex-1">
        <div className="flex items-center gap-2 flex-wrap">
          <span className="font-medium text-sm truncate">{p.name}</span>
          <span className="text-xs rounded-full px-2 py-0.5 bg-gray-100 dark:bg-gray-800 text-gray-600 dark:text-gray-300">{p.source}</span>
          {behind && (
            <span className="text-xs rounded-full px-2 py-0.5 bg-amber-100 dark:bg-amber-900/40 text-amber-800 dark:text-amber-300">
              update available
            </span>
          )}
        </div>
        <div className="mt-0.5 text-xs text-gray-500 dark:text-gray-400 flex items-center gap-2 flex-wrap">
          {ref && <span>{ref}</span>}
          {p.installed_sha && (
            <span title={p.installed_sha}>sha {shortSha(p.installed_sha)}</span>
          )}
          {fetched && <span>fetched {fetched}</span>}
          {p.declares_mcp_servers && (
            <span
              className="inline-flex items-center gap-1"
              title="This plugin declares MCP servers; an add or update here starts them only after the next restart"
            >
              <Icon name="restart_alt" className="w-3.5 h-3.5 shrink-0" />
              MCP servers: apply at restart
            </span>
          )}
          {update?.error && (
            <details className="group min-w-0">
              <summary
                className="inline-flex items-center gap-1 text-amber-600 dark:text-amber-400 cursor-pointer"
                title={`Update check failed: ${update.error}`}
              >
                <Icon name="warning" className="w-3.5 h-3.5 shrink-0" />
                check failed
                <Icon name="expand_more" className="w-3.5 h-3.5 shrink-0 transition-transform group-open:rotate-180" />
              </summary>
              <div className="mt-1 text-amber-700 dark:text-amber-400 break-words whitespace-pre-line font-mono">{update.error}</div>
            </details>
          )}
        </div>
        {p.error && (
          <div className="mt-1 flex items-start gap-1.5 text-xs text-red-600 dark:text-red-400">
            <Icon name="warning" className="w-3.5 h-3.5 shrink-0 mt-0.5" />
            <span className="min-w-0 break-words whitespace-pre-line font-mono">{p.error}</span>
          </div>
        )}
      </div>
      <div className="flex items-center gap-1 shrink-0">
        {p.source === 'github' && (
          <button
            onClick={onUpdate}
            disabled={busy}
            title="Update"
            aria-label={`Update ${p.name}`}
            className="flex items-center justify-center w-11 h-11 rounded-lg text-gray-500 dark:text-gray-400 hover:text-blue-600 dark:hover:text-blue-400 hover:bg-gray-100 dark:hover:bg-gray-700 disabled:opacity-40 transition-colors"
          >
            <Icon name="refresh" className="w-4 h-4" />
          </button>
        )}
        {removable && (
          <button
            onClick={onRemove}
            disabled={busy}
            title="Remove"
            aria-label={`Remove ${p.name}`}
            className="flex items-center justify-center w-11 h-11 rounded-lg text-gray-500 dark:text-gray-400 hover:text-red-600 dark:hover:text-red-400 hover:bg-gray-100 dark:hover:bg-gray-700 disabled:opacity-40 transition-colors"
          >
            <Icon name="delete" className="w-4 h-4" />
          </button>
        )}
      </div>
    </li>
  )
}
