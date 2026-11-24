import { useState } from 'react';
import { getSettings, listModels, setDefaultModel, type ModelInfo } from './api.ts';
import { useLoad } from './useLoad.ts';

const NONE = { models: [] as ModelInfo[], defaultModel: undefined as string | undefined, chosen: undefined as string | undefined };

/** The model sessions run when neither they nor their agent pick one; kept by the server, so it applies on every device. */
export function ModelSetting() {
  const { data, error: loadError, reload } = useLoad(async () => {
    const [list, settings] = await Promise.all([listModels(), getSettings()]);
    return { ...list, chosen: settings.defaultModel };
  }, [], NONE);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');
  const change = (next: string) => {
    setBusy(true);
    setError('');
    setDefaultModel(next || null).then(reload, (e: Error) => setError(e.message)).finally(() => setBusy(false));
  };
  return (
    <fieldset className="m-0 min-w-0 rounded-[18px] border border-line bg-surface p-3.5 shadow-card">
      <legend className="px-1 text-xs font-semibold tracking-wider text-muted uppercase">Model</legend>
      <label htmlFor="default-model" className="m-0 mb-2 block text-[13.5px] text-ink-2">Default for new runs. A session can pick its own in the message box.</label>
      <select id="default-model" value={data.chosen ?? ''} disabled={busy || !data.models.length} onChange={(e) => change(e.target.value)}
        className="focus-ring min-h-11 w-full min-w-0 cursor-pointer rounded-xl border border-line bg-surface px-3 text-[15px] text-ink disabled:cursor-default disabled:opacity-50">
        <option value="">{data.chosen || !data.defaultModel ? 'Server default' : `Server default (${data.defaultModel})`}</option>
        {data.models.map((m) => <option key={m.name} value={m.name}>{m.name}</option>)}
      </select>
      {(error || loadError) && <p role="alert" className="m-0 mt-2 text-[13.5px] text-danger [overflow-wrap:anywhere]">{error || loadError}</p>}
    </fieldset>
  );
}
