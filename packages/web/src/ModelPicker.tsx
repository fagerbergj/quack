import { useState } from 'react';
import { getSessionModel, listModels, setSessionModel, type ModelInfo } from './api.ts';
import { useLoad } from './useLoad.ts';

const NONE = { models: [] as ModelInfo[], defaultModel: undefined as string | undefined, pick: undefined as string | undefined };
const short = (name?: string) => name?.slice(name.indexOf('/') + 1) ?? '';
const context = (m?: ModelInfo) => (m?.contextWindow ? `, ${Math.round(m.contextWindow / 1024)}k context` : '');

/** The session's model for its next run, in the composer; hidden while no model list has loaded. */
export function ModelPicker({ sessionKey }: { sessionKey: string }) {
  const { data, error: loadError } = useLoad(async () => {
    const [list, pick] = await Promise.all([listModels(), getSessionModel(sessionKey)]);
    return { ...list, pick };
  }, [sessionKey], NONE);
  const [pick, setPick] = useState<string | undefined | null>(null);
  const [error, setError] = useState('');
  if (loadError || !data.models.length) return null;
  const value = pick === null ? data.pick ?? '' : pick ?? '';
  const runs = value || data.defaultModel;
  const detail = `Model for the next run: ${runs ?? 'none'}${context(data.models.find((m) => m.name === runs))}${value ? '' : ' (default)'}`;
  const change = (next: string) => {
    setError('');
    setSessionModel(sessionKey, next || null).then(() => setPick(next || undefined), (e: Error) => setError(e.message));
  };
  return (
    <>
      <label htmlFor="model-pick" className="sr-only">Model</label>
      <span id="model-pick-detail" className="sr-only">{error || detail}</span>
      <select id="model-pick" value={value} onChange={(e) => change(e.target.value)} title={error || detail} aria-describedby="model-pick-detail" aria-invalid={error ? true : undefined}
        className={`focus-ring h-9 max-w-[8.5rem] min-w-0 flex-none cursor-pointer truncate rounded-full border-0 bg-transparent px-2 text-[13px] hover:bg-line/40 ${error ? 'text-danger' : 'text-muted'}`}>
        <option value="">Default</option>
        {data.models.map((m) => <option key={m.name} value={m.name}>{short(m.name)}</option>)}
      </select>
    </>
  );
}
