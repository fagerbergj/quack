import { useState, type KeyboardEvent } from 'react';
import { getSessionModel, listModels, setSessionModel, type ModelInfo, type SessionModel } from './api.ts';
import { useLoad } from './useLoad.ts';

const NONE: { models: ModelInfo[]; defaultModel?: string } = { models: [] };
const shortName = (name: string) => name.slice(name.lastIndexOf('/') + 1);

/** The model's full name and context, for a tooltip and screen readers. */
export function modelDetail(name: string | undefined, models: ModelInfo[]) {
  if (!name) return 'none';
  const window = models.find((m) => m.name === name)?.contextWindow;
  return `${name}, ${window ? `${Math.round(window / 1024)}k` : 'unknown'} context`;
}

/** The select's options: each model by its short name, or its full name where two short names match; `current` is kept when the list lost it, so the select never shows another. */
export function modelOptions(models: ModelInfo[], current: string | undefined) {
  const names = models.map((m) => m.name);
  if (current && !names.includes(current)) names.unshift(current);
  const counts = Map.groupBy(names, shortName);
  return names.map((name) => ({ name, label: counts.get(shortName(name))!.length > 1 ? name : shortName(name) }));
}

// Where the shown model comes from: a pick that is still available, the Settings model, or the session's agent.
function sourceOf(now: SessionModel, runs: string, settingsModel: string | undefined) {
  if (now.model === runs) return 'this session\'s pick';
  return runs === settingsModel ? 'from Settings' : 'from its agent';
}

// The picker sits in the composer's form, where Enter on a focused control would send the draft.
const keepDraft = (e: KeyboardEvent) => void (e.key === 'Enter' && e.preventDefault());

/** The session's model for its next run, in the composer; hidden until the model list and the session's model have loaded. */
export function ModelPicker({ sessionKey }: { sessionKey: string }) {
  const { data: list, error: loadError } = useLoad(listModels, [], NONE);
  const { data: loaded } = useLoad(() => getSessionModel(sessionKey), [sessionKey], {});
  const [saved, setSaved] = useState<SessionModel | null>(null);
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState('');
  const now = saved ?? loaded;
  const runs = now.resolvedModel ?? now.model;
  // Until the session's model loads, the select would name its first option instead.
  if (loadError || !list.models.length || !runs) return null;
  const detail = `Model for the next run: ${modelDetail(runs, list.models)}, ${sourceOf(now, runs, list.defaultModel)}`;
  // Choosing the Settings model clears the pick, so the session follows Settings again.
  // shortcut: a session whose agent names its own model then runs that one; no shipped agent names one.
  const change = (next: string) => {
    setError('');
    setSaving(true);
    setSessionModel(sessionKey, next === list.defaultModel ? null : next).then(setSaved, (e: Error) => setError(e.message)).finally(() => setSaving(false));
  };
  const tip = error || detail;
  return (
    <div className="flex items-center gap-1 px-3 text-[11px] leading-4 text-muted">
      <label htmlFor="model-pick">Model:</label>
      <span id="model-pick-detail" className="sr-only">{tip}</span>
      <select id="model-pick" value={runs} disabled={saving} aria-busy={saving || undefined} onChange={(e) => change(e.target.value)} onKeyDown={keepDraft}
        title={tip} aria-describedby="model-pick-detail" aria-invalid={error ? true : undefined}
        className={`focus-ring min-h-8 max-w-[10rem] min-w-0 cursor-pointer truncate rounded border-0 bg-transparent py-0.5 text-[11px] hover:text-ink disabled:cursor-default disabled:opacity-50 ${error ? 'text-danger' : ''}`}>
        {modelOptions(list.models, runs).map((o) => <option key={o.name} value={o.name} title={o.name === list.defaultModel ? 'Settings model; choosing it follows Settings again' : undefined}>{o.label}</option>)}
      </select>
    </div>
  );
}
