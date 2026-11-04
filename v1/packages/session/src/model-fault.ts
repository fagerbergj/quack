import { BACKGROUND_CONTEXT } from '@earendil-works/chord/context';
import { LiveDoc, type Conversation, type Harness } from '@earendil-works/pi-durable';
import { Failures } from '@quack/sdk';

const ctx = BACKGROUND_CONTEXT;
export const MODEL_STEP = 'model';

// pi-durable retries only the errors pi-ai names transient; a provider abort is not one of them.
const isAbort = (detail?: string) => /\babort/i.test(detail ?? '');

async function setRow(root: Conversation, error: string | null) {
  await root.commit(async (tx) => {
    const doc = await tx.doc(Failures, root.id);
    if (error !== null || doc.items[MODEL_STEP]?.error) doc.items[MODEL_STEP] = { error, run: Date.now(), at: Date.now() };
  }, ctx);
}

/** Retries a run a provider abort ended once, and records any other model_error (or a second abort) as a failure so Run again appears. */
export async function watchModelFaults(harness: Harness, root: Conversation, log: (line: string) => void) {
  await root.commit(async (tx) => { await tx.doc(LiveDoc, root.id); }, ctx);
  const watch = await harness.watchDoc(LiveDoc, root.id, ctx);
  // A failure from before open was already shown; only one that ends after open is acted on.
  let seen = String(watch?.value?.runFailure?.taskId ?? '');
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
    if (isAbort(f.detail) && !retrying) {
      retrying = true;
      return void (await root.submit({ type: 'continue' }, ctx));
    }
    retrying = false;
    await setRow(root, f.detail ?? 'The model failed.');
  };
  watch?.start(async () => void onChange().catch((e) => log(`session: model fault handling failed: ${e instanceof Error ? e.message : e}`)));
}

/** Reruns the run a model error ended; null when no such failure is recorded. */
export async function resumeModelFault(root: Conversation, harness: Harness): Promise<{ submissionId: string; status: 'placed' } | null> {
  const row = (await harness.snapshot(Failures, root.id, ctx))?.items[MODEL_STEP];
  if (!row?.error) return null;
  await setRow(root, null);
  const submission = await root.submit({ type: 'continue' }, ctx);
  return { submissionId: String(submission.id), status: 'placed' };
}
