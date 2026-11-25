import { useState, type KeyboardEvent } from 'react';
import { getSessionModel, listModels, setSessionModel, type ModelInfo, type SessionModel } from './api.ts';
import { useMedia } from './useDesktop.ts';
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

const WRAP = 'relative grid h-11 w-11 flex-none place-items-center rounded-full text-ink-2 hover:text-ink has-focus-visible:ring-2 has-focus-visible:ring-accent';

// The phone form: a chip icon the size of the attach button, dotted when the session has its own pick.
const Chip = ({ picked }: { picked: boolean }) => (
  <>
    <svg width="20" height="20" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true"><rect x="6" y="6" width="12" height="12" rx="2" /><path d="M9 2v4M15 2v4M9 18v4M15 18v4M2 9h4M2 15h4M18 9h4M18 15h4" /></svg>
    {picked && <span data-testid="model-picked" className="absolute top-2 right-2 h-2 w-2 rounded-full bg-accent" />}
  </>
);

/** The session's model for its next run, in the composer; hidden while no model list has loaded. */
export function ModelPicker({ sessionKey }: { sessionKey: string }) {
  const { data: list, error: loadError } = useLoad(listModels, [], NONE);
  const { data: loaded } = useLoad(() => getSessionModel(sessionKey), [sessionKey], {});
  const [saved, setSaved] = useState<SessionModel | null>(null);
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState('');
  const narrow = !useMedia('(min-width: 640px)');
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
  const pick = now.model ? runs : `Default (${runs ?? 'none'})`;
  const tip = error || detail;
  return (
    <>
      {!narrow && <label htmlFor="model-pick" className="sr-only">Model</label>}
      <span id="model-pick-detail" className="sr-only">{tip}</span>
      <span className={narrow ? WRAP : 'contents'} title={narrow ? tip : undefined}>
        {narrow && <Chip picked={!!now.model} />}
        <select id="model-pick" value={now.model ?? ''} disabled={saving} aria-busy={saving || undefined} onChange={(e) => change(e.target.value)} onKeyDown={keepDraft}
          title={tip} aria-label={narrow ? `Model: ${pick}` : undefined} aria-describedby="model-pick-detail" aria-invalid={error ? true : undefined}
          className={narrow ? 'absolute inset-0 h-full w-full cursor-pointer opacity-0 disabled:cursor-default' : `focus-ring h-9 max-w-[8.5rem] min-w-0 flex-none cursor-pointer truncate rounded-full border-0 bg-transparent px-2 text-[13px] hover:bg-line/40 disabled:cursor-default disabled:opacity-50 ${error ? 'text-danger' : 'text-muted'}`}>
          <option value="">Default</option>
          {list.models.map((m) => <option key={m.name} value={m.name}>{label(m.name)}</option>)}
        </select>
      </span>
    </>
  );
}
