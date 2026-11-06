import { useId, useState, type ReactNode, type SubmitEvent } from 'react';
import { Dialog } from './Dialog.tsx';

const btn = 'min-h-11 cursor-pointer rounded-full border px-4 text-[14px] font-medium lg:min-h-9';

export function Modal({ title, onClose, onSubmit, children, action, danger, busy, error }: { title: string; onClose: () => void; onSubmit: () => void; children: ReactNode; action: string; danger?: boolean; busy: boolean; error: string }) {
  const id = useId();
  const submit = (e: SubmitEvent) => {
    e.preventDefault();
    onSubmit();
  };
  return (
    <Dialog labelledBy={id} onClose={onClose} className="fixed inset-x-3 top-1/2 z-50 mx-auto max-w-md -translate-y-1/2 rounded-2xl border border-line bg-canvas p-4 shadow-lift">
      <form onSubmit={submit} className="flex flex-col gap-3">
        <h2 id={id} className="m-0 font-sans text-base font-semibold">{title}</h2>
        {children}
        {error && <p role="alert" className="m-0 text-[13px] text-danger">{error}</p>}
        <div className="flex justify-end gap-2">
          <button type="button" onClick={onClose} className={`${btn} border-line bg-transparent text-ink-2 hover:bg-soft`}>Cancel</button>
          <button type="submit" disabled={busy} className={`${btn} border-0 disabled:opacity-50 ${danger ? 'bg-danger text-canvas' : 'bg-ink text-on-ink'}`}>{action}</button>
        </div>
      </form>
    </Dialog>
  );
}

// Runs the server call; a refusal stays in the dialog so the person sees why.
export function useAction(run: () => Promise<unknown>, done: () => void) {
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');
  const go = () => {
    setBusy(true);
    run().then(done, (e: Error) => (setError(e.message), setBusy(false)));
  };
  return { busy, error, go };
}

export function RenameDialog({ current, save, onClose }: { current: string; save: (title: string) => Promise<unknown>; onClose: () => void }) {
  const [title, setTitle] = useState(current);
  const { busy, error, go } = useAction(() => save(title), onClose);
  return (
    <Modal title="Rename session" onClose={onClose} onSubmit={go} action="Save" busy={busy} error={error}>
      <label className="flex flex-col gap-1 text-[13px] text-muted">
        Title
        <input autoFocus value={title} maxLength={120} onChange={(e) => setTitle(e.target.value)} className="min-h-11 rounded-xl border border-line bg-surface px-3 text-[15px] text-ink outline-none focus:shadow-ring" />
      </label>
      <p className="m-0 text-xs text-muted">Leave it empty to use the first message.</p>
    </Modal>
  );
}

export function DeleteDialog({ name, remove, onClose, onDone }: { name: string; remove: () => Promise<unknown>; onClose: () => void; onDone: () => void }) {
  const { busy, error, go } = useAction(remove, onDone);
  return (
    <Modal title="Delete session?" onClose={onClose} onSubmit={go} action="Delete" danger busy={busy} error={error}>
      <p className="m-0 text-[14px] [overflow-wrap:anywhere]">
        <strong>{name}</strong> and its transcript, artifacts and workspace will be deleted for good.
      </p>
    </Modal>
  );
}
