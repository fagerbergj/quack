import { BACKGROUND_CONTEXT } from '@earendil-works/chord/context';
import { ConversationBusy, LiveDoc, type Conversation, type Harness } from '@earendil-works/pi-durable';
import { Failures } from '@quack/sdk';

const ctx = BACKGROUND_CONTEXT;
export const MODEL_STEP = 'model';

// pi-durable retries only the errors pi-ai names transient; a provider abort is not one of them.
const isAbort = (detail?: string) => /finish_reason:\s*abort/i.test(detail ?? '');

async function setRow(root: Conversation, error: string | null, run = 0) {
  await root.commit(async (tx) => {
    const doc = await tx.doc(Failures, root.id);
    if (error !== null || doc.items[MODEL_STEP]?.error) doc.items[MODEL_STEP] = { error, run, at: Date.now() };
  }, ctx);
}

/** Retries a run a provider abort ended once, and records any other model_error (or a second abort) as a failure so Run again appears. */
export async function watchModelFaults(harness: Harness, root: Conversation, log: (line: string) => void) {
  await root.commit(async (tx) => { await tx.doc(LiveDoc, root.id); }, ctx);
  const watch = await harness.watchDoc(LiveDoc, root.id, ctx);
  // A failure present at open is recorded, not retried: a crash after its commit must not leave the session idle with nothing to resume.
  const first = watch?.value?.runFailure;
  let seen = String(first?.taskId ?? '');
  if (first?.reason === 'model_error') await setRow(root, first.detail ?? 'The model failed.', Number(first.taskId) || 0);
  let retrying = false;
  const onChange = async () => {
    const live = watch?.value;
    const f = live?.runFailure;
    if (!f) {
      if (!live?.run) retrying = false;
      return void (await setRow(root, null));
    }
    if (f.reason !== 'model_error' || String(f.taskId) === seen) return;
    seen = String(f.taskId);
    if (isAbort(f.detail) && !retrying && !live?.run) {
      retrying = true;
      return void (await root.submit({ type: 'continue' }, ctx));
    }
    retrying = false;
    await setRow(root, f.detail ?? 'The model failed.', Number(f.taskId) || 0);
  };
  watch?.start(async () => void onChange().catch((e) => log(`session: model fault handling failed: ${e instanceof Error ? e.message : e}`)));
}

/** Reruns the run a model error ended; null when no such failure is recorded. */
export async function resumeModelFault(root: Conversation, harness: Harness): Promise<{ submissionId: string; status: 'placed' } | null> {
  const row = (await harness.snapshot(Failures, root.id, ctx))?.items[MODEL_STEP];
  if (!row?.error) return null;
  // The rerun's start commit clears the row through the watch; a busy conversation is a 409, not a 500.
  try {
    return { submissionId: String((await root.submit({ type: 'continue' }, ctx)).id), status: 'placed' };
  } catch (e) {
    if (e instanceof ConversationBusy) return null;
    throw e;
  }
}
