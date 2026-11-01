import assert from 'node:assert/strict';
import { mkdtemp, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { test } from 'node:test';
import { BACKGROUND_CONTEXT as ctx } from '@earendil-works/chord/context';
import { createModels, fauxAssistantMessage, fauxProvider } from '@earendil-works/pi-ai';
import { Artifacts } from '@quack/sdk';
import { openSession } from '@quack/session';
import { runWorkflow, workflowExtension } from '@quack/workflows';
import { classifierChoice, createIngestService, createRemarkableService, ingestWorkflow, NONE, PAGE_WIDTH, parseTopic, problems, renderPages, type IngestService, type Transcript } from '../src/index.ts';

// A real two-page PDF, written by hand so the render step runs the actual library.
function pdf(pages: number): Uint8Array {
  const objs = ['<< /Type /Catalog /Pages 2 0 R >>', `<< /Type /Pages /Kids [${Array.from({ length: pages }, (_, i) => `${4 + i * 2} 0 R`).join(' ')}] /Count ${pages} >>`, '<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>'];
  for (let i = 0; i < pages; i++) {
    const body = `BT /F1 24 Tf 40 700 Td (page ${i + 1}) Tj ET`;
    objs.push(`<< /Type /Page /Parent 2 0 R /MediaBox [0 0 445 594] /Resources << /Font << /F1 3 0 R >> >> /Contents ${5 + i * 2} 0 R >>`, `<< /Length ${body.length} >>\nstream\n${body}\nendstream`);
  }
  let out = '%PDF-1.4\n';
  const at = objs.map((o, i) => {
    const pos = out.length;
    out += `${i + 1} 0 obj\n${o}\nendobj\n`;
    return pos;
  });
  const xref = out.length;
  out += `xref\n0 ${objs.length + 1}\n0000000000 65535 f \n${at.map((p) => `${String(p).padStart(10, '0')} 00000 n \n`).join('')}trailer\n<< /Size ${objs.length + 1} /Root 1 0 R >>\nstartxref\n${xref}\n%%EOF\n`;
  return new TextEncoder().encode(out);
}

// The service half's state, held in memory: what outlives a session is the topic list.
function fakeService(topics: string[] = []): IngestService & { list: string[] } {
  return { list: topics, pdf: async () => pdf(2), topics: async () => [...topics], addTopic: async (n) => void (topics.some((t) => t.toLowerCase() === n.toLowerCase()) || topics.push(n)) };
}

type Answers = { type: string; topic: string; proposal?: string };

// A model that answers by what it is asked: an image is a page to transcribe, the rest are the decision points and the topic proposal.
function fakeModel(answers: Answers) {
  const calls: string[] = [];
  const faux = fauxProvider({ models: [{ id: 'faux-1' }] });
  faux.setResponses(Array.from({ length: 20 }, () => (c: { messages: Array<{ content: unknown }> }) => {
    const last = c.messages.at(-1)!.content;
    if (Array.isArray(last)) {
      const [{ text }, image] = last as Array<{ text: string; type: string; mimeType?: string }>;
      assert.equal(image.mimeType, 'image/jpeg');
      calls.push(`transcribe ${text}`);
      return fauxAssistantMessage(`Handwritten words of ${text.slice(0, -1)}`);
    }
    const ask = String(last);
    const choose = (choice: string) => fauxAssistantMessage(JSON.stringify({ choice, confidence: 0.9, reason: 'fits' }));
    if (ask.includes('What kind of')) return calls.push('type'), choose(answers.type);
    if (ask.includes('Which existing topic')) return calls.push('topic'), choose(answers.topic);
    calls.push('propose');
    return fauxAssistantMessage(answers.proposal ?? 'unused');
  }));
  const models = createModels();
  models.setProvider(faux.provider);
  const m = faux.getModel();
  return { models, ref: { provider: m.provider, modelId: m.id }, calls };
}

async function ingest(service: IngestService, answers: Answers, runs: Array<{ version: string; request?: string }> = [{ version: 'v1' }]) {
  const { models, ref, calls } = fakeModel(answers);
  const dir = await mkdtemp(join(tmpdir(), 'quack-rm-'));
  const workflow = ingestWorkflow({ service, visionModel: ref });
  const s = await openSession({ dir, models, agent: { model: ref, instructions: 'You transcribe.', extensions: [workflowExtension([workflow])] } });
  try {
    const outcomes = [];
    for (const r of runs) outcomes.push(await runWorkflow(s.harness, s.root, workflow, JSON.stringify({ id: 'n1', name: 'Notes', version: r.version }), r.request ?? r.version));
    const sealed = (await s.harness.snapshot(Artifacts, s.root.id, ctx))?.kinds['remarkable.transcript']?.versions ?? [];
    return { outcomes, sealed, calls, transcript: (i = 0) => JSON.parse(sealed[i].content) as Transcript };
  } finally {
    await s.close();
    await rm(dir, { recursive: true, force: true });
  }
}

test('a two-page document is rendered, transcribed per page and sealed with its type and topic', async () => {
  const service = fakeService();
  const r = await ingest(service, { type: 'meeting', topic: NONE, proposal: '"Home Renovation".' });
  assert.equal(r.outcomes[0].status, 'completed', JSON.stringify(r.outcomes[0]));
  assert.equal(r.sealed.length, 1);
  assert.deepEqual(r.transcript(), {
    kind: 'remarkable.transcript',
    document: { id: 'n1', name: 'Notes', version: 'v1' },
    type: 'meeting',
    topic: 'Home Renovation',
    pages: [{ page: 1, text: 'Handwritten words of Page 1 of 2' }, { page: 2, text: 'Handwritten words of Page 2 of 2' }],
  });
  assert.deepEqual(r.calls, ['transcribe Page 1 of 2.', 'transcribe Page 2 of 2.', 'type', 'topic', 'propose']);
});

test('an existing topic wins when it fits, and nothing is proposed', async () => {
  const service = fakeService(['home renovation', 'taxes']);
  const r = await ingest(service, { type: 'todo-list', topic: 'taxes' });
  assert.equal(r.transcript().topic, 'taxes');
  assert.equal(r.transcript().type, 'todo-list');
  assert.ok(!r.calls.includes('propose'));
  assert.deepEqual(service.list, ['home renovation', 'taxes']);
});

test('none of these proposes a topic that joins the list for the next document', async () => {
  const service = fakeService(['taxes']);
  const first = await ingest(service, { type: 'idea', topic: NONE, proposal: 'garden plan' });
  assert.equal(first.transcript().topic, 'garden plan');
  assert.deepEqual(service.list, ['taxes', 'garden plan']);
  const next = await ingest(service, { type: 'journal', topic: 'garden plan' });
  assert.equal(next.transcript().topic, 'garden plan');
  assert.ok(!next.calls.includes('propose'));
});

test('a re-ingest of the same version joins the sealed run: no model call, one version, one topic', async () => {
  const service = fakeService();
  const r = await ingest(service, { type: 'sketch', topic: NONE, proposal: 'bike frame' }, [{ version: 'v1' }, { version: 'v1' }]);
  assert.deepEqual(r.outcomes.map((o) => o.status), ['completed', 'completed']);
  assert.equal(r.sealed.length, 1);
  assert.equal(r.calls.filter((c) => c === 'propose').length, 1);
  assert.equal(r.calls.length, 5);
  assert.deepEqual(service.list, ['bike frame']);
});

test('a page the model returns empty fails the transcribe step and seals nothing', async () => {
  const service = fakeService();
  const faux = fauxProvider({ models: [{ id: 'faux-1' }] });
  faux.setResponses([fauxAssistantMessage('  ')]);
  const models = createModels();
  models.setProvider(faux.provider);
  const m = faux.getModel();
  const dir = await mkdtemp(join(tmpdir(), 'quack-rm-'));
  const workflow = ingestWorkflow({ service });
  const s = await openSession({ dir, models, agent: { model: { provider: m.provider, modelId: m.id }, extensions: [workflowExtension([workflow])] } });
  try {
    const outcome = await runWorkflow(s.harness, s.root, workflow, JSON.stringify({ id: 'n1', name: 'Notes', version: 'v1' }), 'v1');
    assert.equal(outcome.status, 'failed');
    assert.match(outcome.status === 'failed' ? outcome.error.message : '', /page 1 came back empty/);
    assert.equal((await s.harness.snapshot(Artifacts, s.root.id, ctx))?.kinds['remarkable.transcript'], undefined);
  } finally {
    await s.close();
    await rm(dir, { recursive: true, force: true });
  }
});

test('render gives one JPEG per page at the tablet width', async () => {
  const pages = await renderPages(pdf(2));
  assert.equal(pages.length, 2);
  const { loadImage } = await import('@napi-rs/canvas');
  for (const p of pages) assert.equal((await loadImage(Buffer.from(p))).width, PAGE_WIDTH);
});

test('the transcript check names every page without text and a wrong type', () => {
  const t: Transcript = { kind: 'remarkable.transcript', document: { id: 'n1', name: 'N', version: '1' }, type: 'poem', topic: 'x', pages: [{ page: 1, text: 'a' }, { page: 3, text: ' ' }] };
  assert.deepEqual(problems(t), ['type must be one of meeting, idea, todo-list, journal, sketch, reading-notes', 'page 2 is numbered 3', 'page 2 has no text']);
});

test('parseTopic keeps a short name, reuses an existing one by case, and refuses a sentence', () => {
  assert.equal(parseTopic('"Home renovation."\nbecause', []), 'Home renovation');
  assert.equal(parseTopic('TAXES', ['taxes']), 'taxes');
  assert.throws(() => parseTopic('This document is about many different things at once', []), /short topic/);
});

test('the classifier resolver answers from the decision model, and escalates when it is over its input cap', async () => {
  const point = { name: 'type', question: 'What kind?', options: ['meeting', 'idea'], evidence: 'x'.repeat(100) };
  let asked: unknown;
  const model = { id: 'clef', type: 'classifier' };
  const api = { models: { getAllModels: () => [model], classify: async (_m: unknown, c: unknown) => ((asked = c), { stopReason: 'stop', answers: { pick: { type: 'choice', choice: 'idea', probabilities: { idea: 0.9 }, confidence: 0.9 } } }) } };
  const ref = { provider: 'clef', modelId: 'clef', maxInputTokens: 8192 };
  const got = await classifierChoice(ref, (o) => `is ${o}`).resolve(point, api as never, ctx);
  assert.equal(got?.choice, 'idea');
  assert.deepEqual((asked as { questions: { pick: { criteria: unknown } } }).questions.pick.criteria, { meeting: 'is meeting', idea: 'is idea' });
  assert.equal(await classifierChoice({ ...ref, maxInputTokens: 10 }, String).resolve(point, api as never, ctx), undefined);
});

test('the service serves the PDF and keeps the topic list, and the ingest client reads both', async () => {
  const service = createRemarkableService({ serviceToken: 't', dbPath: ':memory:', distributorUrl: 'http://unused', agent: 'a', client: { tree: async () => [], documents: async () => [], downloadPdf: async (id) => Buffer.from(id === 'n 1' ? pdf(1) : '') } });
  await new Promise<void>((r) => service.listen(0, r));
  try {
    const url = `http://127.0.0.1:${(service.address() as { port: number }).port}`;
    const client = createIngestService(url, 't');
    assert.deepEqual(await client.topics(), []);
    await client.addTopic('  Home   renovation ');
    await client.addTopic('home RENOVATION');
    assert.deepEqual(await client.topics(), ['Home renovation']);
    assert.equal((await renderPages(await client.pdf('n 1'))).length, 1);
    const denied = await fetch(`${url}/topics`);
    assert.equal(denied.status, 401);
    assert.equal((await fetch(`${url}/topics`, { method: 'POST', headers: { authorization: 'Bearer t' }, body: '{"name":""}' })).status, 400);
  } finally {
    await service.stop();
  }
});
