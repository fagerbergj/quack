import { mkdtemp, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { BACKGROUND_CONTEXT as ctx } from '@earendil-works/chord/context';
import { createModels, fauxAssistantMessage, fauxProvider } from '@earendil-works/pi-ai';
import { openSession } from '@quack/session';
import { runWorkflow, WorkflowDoc, workflowExtension, type Workflow } from '@quack/workflows';

/** Runs a job whose model host refuses the call, as an overloaded host does; returns the outcome and the failed step. */
export async function refusedRun(workflow: Workflow, input: object) {
  const faux = fauxProvider({ models: [{ id: 'faux-1' }] });
  faux.setResponses([fauxAssistantMessage('', { stopReason: 'error', errorMessage: '503 no slot free' })]);
  const models = createModels();
  models.setProvider(faux.provider);
  const dir = await mkdtemp(join(tmpdir(), 'quack-refused-'));
  const m = faux.getModel();
  const s = await openSession({ dir, models, agent: { model: { provider: m.provider, modelId: m.id }, extensions: [workflowExtension([workflow])] } });
  try {
    const outcome = await runWorkflow(s.harness, s.root, workflow, JSON.stringify(input));
    return { outcome, failed: (await s.harness.snapshot(WorkflowDoc, s.root.id, ctx))!.steps.find((x) => x.status === 'failed') };
  } finally {
    await s.close();
    await rm(dir, { recursive: true, force: true });
  }
}
