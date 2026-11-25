import { useState } from 'react';
import { listModels, setDefaultModel, type ModelInfo } from './api.ts';
import { modelDetail, modelOptions } from './ModelPicker.tsx';
import { useLoad } from './useLoad.ts';

const NONE = { models: [] as ModelInfo[], defaultModel: undefined as string | undefined };

/** The model sessions run when neither they nor their agent pick one; kept by the server, so it applies on every device. */
export function ModelSetting() {
  const { data, error: loadError, reload } = useLoad(listModels, [], NONE);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');
  const change = (next: string) => {
    setBusy(true);
    setError('');
    setDefaultModel(next).then(reload, (e: Error) => setError(e.message)).finally(() => setBusy(false));
  };
  const current = data.defaultModel;
  return (
    <fieldset className="m-0 min-w-0 rounded-[18px] border border-line bg-surface p-3.5 shadow-card">
      <legend className="px-1 text-xs font-semibold tracking-wider text-muted uppercase">Model</legend>
      <label htmlFor="default-model" className="m-0 mb-2 block text-[13.5px] text-ink-2">Default for new runs. A session can pick its own in the message box.</label>
      <select id="default-model" value={current ?? ''} disabled={busy || !data.models.length} onChange={(e) => change(e.target.value)}
        title={modelDetail(current, data.models)} aria-describedby="default-model-detail" className="focus-ring min-h-11 w-full min-w-0 cursor-pointer rounded-xl border border-line bg-surface px-3 text-[15px] text-ink disabled:cursor-default disabled:opacity-50">
        {modelOptions(data.models, current).map((o) => <option key={o.name} value={o.name}>{o.label}</option>)}
      </select>
      <span id="default-model-detail" className="sr-only">{modelDetail(current, data.models)}</span>
      {(error || loadError) && <p role="alert" className="m-0 mt-2 text-[13.5px] text-danger [overflow-wrap:anywhere]">{error || loadError}</p>}
    </fieldset>
  );
}
