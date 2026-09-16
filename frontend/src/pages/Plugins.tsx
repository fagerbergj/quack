import { useEffect, useState, useCallback } from 'react'
import { api, type Plugin, type PluginUpdate } from '../api'
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
}

function shortSha(sha?: string): string {
  return sha ? sha.slice(0, 7) : '—'
}

// The dynamic plugin registry page (epic #1427 P2): list, add, remove, and
// update quack's skill plugins. Routed at /plugins - see App.tsx/router.ts.
export default function Plugins({ navOpen, onToggleNav, initialPlugins, initialUpdates }: PluginsProps) {
  const [plugins, setPlugins] = useState<Plugin[]>(initialPlugins ?? [])
  const [updates, setUpdates] = useState<Map<string, PluginUpdate>>(
    new Map((initialUpdates ?? []).map(u => [u.name, u])),
  )
  const [loading, setLoading] = useState(initialPlugins === undefined)
  const [loadError, setLoadError] = useState<string | null>(null)
  const [entry, setEntry] = useState('')
  const [addError, setAddError] = useState<string | null>(null)
  const [adding, setAdding] = useState(false)
  const [busy, setBusy] = useState<Set<string>>(new Set())

  const load = useCallback(() => {
    if (initialPlugins !== undefined) return // story/test seam
    let cancelled = false
    setLoading(true)
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
      .catch(e => { if (!cancelled) setLoadError(e instanceof Error ? e.message : 'Failed to load plugins') })
      .finally(() => { if (!cancelled) setLoading(false) })
    return () => { cancelled = true }
  }, [initialPlugins])

  useEffect(() => { load() }, [load])

  async function handleAdd(e: React.FormEvent) {
    e.preventDefault()
    if (!entry.trim()) return
    setAdding(true)
    setAddError(null)
    try {
      await api.createPlugin(entry.trim())
      setEntry('')
      load()
    } catch (err) {
      setAddError(err instanceof Error ? err.message : 'Failed to add plugin')
    } finally {
      setAdding(false)
    }
  }

  async function handleRemove(name: string) {
    if (!window.confirm(`Remove plugin "${name}"? This deletes its clone on disk.`)) return
    setBusy(prev => new Set(prev).add(name))
    try {
      await api.deletePlugin(name)
      load()
    } catch (err) {
      setLoadError(err instanceof Error ? err.message : `Failed to remove ${name}`)
    } finally {
      setBusy(prev => { const next = new Set(prev); next.delete(name); return next })
    }
  }

  async function handleUpdate(name: string) {
    setBusy(prev => new Set(prev).add(name))
    try {
      await api.updatePlugin(name)
      load()
    } catch (err) {
      setLoadError(err instanceof Error ? err.message : `Failed to update ${name}`)
    } finally {
      setBusy(prev => { const next = new Set(prev); next.delete(name); return next })
    }
  }

  async function handleUpdateAll() {
    setBusy(prev => new Set(prev).add('*'))
    try {
      await api.updateAllPlugins()
      load()
    } catch (err) {
      setLoadError(err instanceof Error ? err.message : 'Failed to update plugins')
    } finally {
      setBusy(prev => { const next = new Set(prev); next.delete('*'); return next })
    }
  }

  const anyBehind = [...updates.values()].some(u => u.behind)

  return (
    <div className="flex flex-col h-full bg-gray-50 dark:bg-gray-900 text-gray-900 dark:text-white">
      <div className="flex items-center gap-3 px-4 py-3 sm:px-6 border-b border-gray-200 dark:border-gray-700 bg-white dark:bg-gray-800">
        <NavToggle open={navOpen} onToggle={onToggleNav} />
        <h1 className="text-base font-semibold text-gray-900 dark:text-white flex-1">Plugins</h1>
        <button
          onClick={() => void handleUpdateAll()}
          disabled={!anyBehind || busy.has('*')}
          className="flex items-center gap-1.5 rounded-md px-3 py-1.5 text-sm font-medium text-blue-700 dark:text-blue-400 bg-blue-50 dark:bg-blue-900/30 hover:bg-blue-100 dark:hover:bg-blue-900/50 disabled:opacity-40 disabled:cursor-not-allowed transition-colors"
        >
          <Icon name="refresh" className="w-4 h-4" />
          Update all
        </button>
      </div>

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
            className="rounded-md px-3 py-1.5 text-sm font-medium text-white bg-blue-600 hover:bg-blue-700 disabled:opacity-40 disabled:cursor-not-allowed transition-colors"
          >
            Add
          </button>
        </div>
        {addError && (
          <div className="flex items-center gap-1.5 text-sm text-red-600 dark:text-red-400">
            <Icon name="warning" className="w-4 h-4 shrink-0" />
            <span>{addError}</span>
          </div>
        )}
      </form>

      <div className="flex-1 overflow-y-auto overscroll-contain">
        {loading && (
          <div className="text-center text-gray-500 dark:text-gray-400 text-sm py-10">Loading…</div>
        )}
        {!loading && loadError && (
          <div className="m-3 rounded-md bg-red-50 dark:bg-red-950/30 border border-red-200 dark:border-red-800 px-4 py-3 text-sm text-red-700 dark:text-red-400">
            {loadError}
          </div>
        )}
        {!loading && !loadError && plugins.length === 0 && (
          <div className="text-center text-gray-500 dark:text-gray-400 text-sm py-10">No plugins registered</div>
        )}
        {!loading && !loadError && plugins.length > 0 && (
          <ul className="divide-y divide-gray-200 dark:divide-gray-700">
            {plugins.map(p => (
              <PluginRow
                key={p.name}
                plugin={p}
                update={updates.get(p.name)}
                busy={busy.has(p.name)}
                onUpdate={() => void handleUpdate(p.name)}
                onRemove={() => void handleRemove(p.name)}
              />
            ))}
          </ul>
        )}
      </div>
    </div>
  )
}

function PluginRow({ plugin: p, update, busy, onUpdate, onRemove }: {
  plugin: Plugin
  update?: PluginUpdate
  busy: boolean
  onUpdate: () => void
  onRemove: () => void
}) {
  const removable = p.source !== 'embedded'
  const behind = update?.behind ?? false
  const fetched = relativeTime(p.fetched_at)

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
          <span>{p.ref ?? 'default branch'}</span>
          {p.installed_sha && (
            <span title={p.installed_sha}>sha {shortSha(p.installed_sha)}</span>
          )}
          {fetched && <span>fetched {fetched}</span>}
        </div>
        {p.error && (
          <div className="mt-1 flex items-center gap-1.5 text-xs text-red-600 dark:text-red-400">
            <Icon name="warning" className="w-3.5 h-3.5 shrink-0" />
            <span className="truncate">{p.error}</span>
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
            className="flex items-center justify-center w-9 h-9 rounded-lg text-gray-500 dark:text-gray-400 hover:text-blue-600 dark:hover:text-blue-400 hover:bg-gray-100 dark:hover:bg-gray-700 disabled:opacity-40 transition-colors"
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
            className="flex items-center justify-center w-9 h-9 rounded-lg text-gray-500 dark:text-gray-400 hover:text-red-600 dark:hover:text-red-400 hover:bg-gray-100 dark:hover:bg-gray-700 disabled:opacity-40 transition-colors"
          >
            <Icon name="delete" className="w-4 h-4" />
          </button>
        )}
      </div>
    </li>
  )
}
