import { useState, type KeyboardEvent } from 'react';
import { getSessionModel, listModels, setSessionModel, type ModelInfo, type SessionModel } from './api.ts';
import { useLoad } from './useLoad.ts';

const NONE = { models: [] as ModelInfo[] };
const context = (m?: ModelInfo) => (m?.contextWindow ? `, ${Math.round(m.contextWindow / 1024)}k context` : '');
const idOf = (name: string) => name.slice(name.indexOf('/') + 1);

// The model id alone, unless another provider offers the same id: then provider/model, so no two options read alike.
function labels(models: ModelInfo[]) {
  const counts = Map.groupBy(models, (m) => idOf(m.name));
  return (name: string) => (counts.get(idOf(name))!.length > 1 ? name : idOf(name));
}

// The picker sits in the composer's form, where Enter on a focused control would send the draft.
const keepDraft = (e: KeyboardEvent) => void (e.key === 'Enter' && e.preventDefault());

/** The session's model for its next run, in the composer; hidden while no model list has loaded. */
export function ModelPicker({ sessionKey }: { sessionKey: string }) {
  const { data: list, error: loadError } = useLoad(listModels, [], NONE);
  const { data: loaded } = useLoad(() => getSessionModel(sessionKey), [sessionKey], {});
  const [saved, setSaved] = useState<SessionModel | null>(null);
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState('');
  if (loadError || !list.models.length) return null;
  const now = saved ?? loaded;
  const runs = now.resolvedModel;
  const detail = `Model for the next run: ${runs ?? 'none'}${context(list.models.find((m) => m.name === runs))}${now.model ? '' : ' (default)'}`;
  const label = labels(list.models);
  const change = (next: string) => {
    setError('');
    setSaving(true);
    setSessionModel(sessionKey, next || null).then(setSaved, (e: Error) => setError(e.message)).finally(() => setSaving(false));
  };
  return (
    <>
      <label htmlFor="model-pick" className="sr-only">Model</label>
      <span id="model-pick-detail" className="sr-only">{error || detail}</span>
      <select id="model-pick" value={now.model ?? ''} disabled={saving} aria-busy={saving || undefined} onChange={(e) => change(e.target.value)} onKeyDown={keepDraft}
        title={error || detail} aria-describedby="model-pick-detail" aria-invalid={error ? true : undefined}
        className={`focus-ring h-9 max-w-[8.5rem] min-w-0 flex-none cursor-pointer truncate rounded-full border-0 bg-transparent px-2 text-[13px] hover:bg-line/40 disabled:cursor-default disabled:opacity-50 ${error ? 'text-danger' : 'text-muted'}`}>
        <option value="">Default</option>
        {list.models.map((m) => <option key={m.name} value={m.name}>{label(m.name)}</option>)}
      </select>
    </>
  );
}
