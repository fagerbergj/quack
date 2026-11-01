import type { JsonValue } from '@earendil-works/chord';
import { contentText } from '@earendil-works/pi-ai';
import type { ClassifierRef, ModelRef } from '@quack/judge';
import { agentModel, seal, type Decision } from '@quack/sdk';
import { defineWorkflow, type StepContext } from '@quack/workflows';
import { classifierChoice, parseTopic } from './classify.ts';
import { renderPages } from './render.ts';
import { TYPES, transcriptKind, type Transcript } from './transcript.ts';

export const INGEST_WORKFLOW = 'remarkable.ingest';
export const NONE = 'none of these';

/** The service half's side of ingest: the document PDF and the durable topic list. */
export type IngestService = {
  pdf(id: string): Promise<Uint8Array>;
  topics(): Promise<string[]>;
  addTopic(name: string): Promise<void>;
};

export type IngestDeps = {
  service: IngestService;
  /** The vision-capable model named in the agent definition; the agent's own model when unset. */
  visionModel?: ModelRef;
  /** A decision model such as Clef, asked first for type and topic. */
  classifier?: ClassifierRef;
};

type Doc = { id: string; name: string; version: string };

// The bar the judge applies to Clef; below it the chain falls through to the session model, then a person.
const MIN_CONFIDENCE = 0.8;
// Clef's input cap is 8192 tokens; the opening pages carry what a type or topic needs.
const EXCERPT_CHARS = 6000;
const TYPE_HINT: Record<string, string> = {
  meeting: 'notes taken during a meeting or call',
  idea: 'an idea, plan or brainstorm',
  'todo-list': 'a list of tasks to do',
  journal: 'a personal journal or diary entry',
  sketch: 'mostly drawings or diagrams',
  'reading-notes': 'notes on a book, article or talk',
};

const pageFile = (n: number) => `page-${n}.jpg`;
const step = <T extends JsonValue>(c: StepContext, name: string) => c.prior[name] as T;
const text = (c: StepContext) => step<string[]>(c, 'transcribe').map((t, i) => `Page ${i + 1}\n${t}`).join('\n\n');

async function must<T>(read: Promise<{ ok: true; value: T } | { ok: false; error: { message?: string; code?: string } }>, what: string): Promise<T> {
  const r = await read;
  if (!r.ok) throw new Error(`${what}: ${r.error.message ?? r.error.code ?? 'failed'}`);
  return r.value;
}

async function transcribe(c: StepContext, deps: IngestDeps): Promise<JsonValue> {
  const agent = await c.api.agent(c.ctx);
  const ref = deps.visionModel ?? agent.model;
  const model = ref && c.api.models.getModel(ref.provider, ref.modelId);
  if (!model) throw new Error('step transcribe: the vision model is not available');
  const count = step<{ pages: number }>(c, 'render').pages;
  const pages: string[] = [];
  for (let n = 1; n <= count; n++) {
    // Memoized per page, so a crash resumes at the next page instead of re-reading finished ones.
    const done = await c.api.memo<string>(`transcribe:${n}`, c.ctx);
    if (done !== undefined) {
      pages.push(done);
      continue;
    }
    const data = Buffer.from(await must(c.api.env!.readBinaryFile(pageFile(n), c.ctx), pageFile(n))).toString('base64');
    const content = [{ type: 'text' as const, text: `Page ${n} of ${count}.` }, { type: 'image' as const, data, mimeType: 'image/jpeg' }];
    const reply = await c.api.models.completeSimple(model, { systemPrompt: agent.instructions ?? undefined, messages: [{ role: 'user', content, timestamp: c.api.now() }] }, { signal: c.signal });
    const page = contentText(reply.content).trim();
    if (!page) throw new Error(`step transcribe: page ${n} came back empty`);
    pages.push(await c.api.memo<string>(`transcribe:${n}`, page, c.ctx));
  }
  return pages;
}

async function propose(c: StepContext): Promise<string> {
  const existing = step<string[]>(c, 'topics');
  const chosen = step<Decision>(c, 'topic').choice;
  if (chosen !== NONE) return chosen;
  const agent = await c.api.agent(c.ctx);
  const model = agent.model && c.api.models.getModel(agent.model.provider, agent.model.modelId);
  if (!model) throw new Error('step propose: the session model is not available');
  const ask = `Name the topic of this handwritten document in 2 to 4 words, such as "home renovation" or "quarterly planning". Existing topics, none of which fit: ${existing.join(', ') || 'none'}. Answer with the name only.\n\n${text(c).slice(0, EXCERPT_CHARS)}`;
  const made = await c.api.memo<string>('topic-proposal', c.ctx);
  if (made !== undefined) return made;
  const reply = await c.api.models.completeSimple(model, { messages: [{ role: 'user', content: ask, timestamp: c.api.now() }] }, { signal: c.signal });
  return c.api.memo<string>('topic-proposal', parseTopic(contentText(reply.content), existing), c.ctx);
}

/** Ingest (RMK-2, RMK-3): render pages, transcribe each, classify by type and topic, then seal the transcript. */
export function ingestWorkflow(deps: IngestDeps) {
  const doc = (c: StepContext) => JSON.parse(String(c.input)) as Doc;
  const resolvers = (describe: (o: string) => string) => [...(deps.classifier ? [classifierChoice(deps.classifier, describe)] : []), agentModel];
  return defineWorkflow({
    name: INGEST_WORKFLOW,
    steps: [
      {
        name: 'render',
        kind: 'code',
        run: async (c) => {
          const { id, name } = doc(c);
          const pdf = await deps.service.pdf(id).catch((err: Error) => {
            throw new Error(`rmfakecloud could not export ${name}: ${err.message}`);
          });
          const images = await renderPages(pdf);
          if (!images.length) throw new Error('step render: the document has no pages');
          for (const [i, image] of images.entries()) await must(c.api.env!.writeFile(pageFile(i + 1), image, c.ctx), pageFile(i + 1));
          return { pages: images.length };
        },
      },
      { name: 'transcribe', kind: 'code', run: (c) => transcribe(c, deps) },
      { name: 'type', kind: 'decision', resolvers: resolvers((o) => TYPE_HINT[o] ?? o), point: (c) => ({ question: 'What kind of handwritten document is this?', options: [...TYPES], minConfidence: MIN_CONFIDENCE, evidence: text(c).slice(0, EXCERPT_CHARS) }) },
      { name: 'topics', kind: 'code', run: async () => deps.service.topics() },
      {
        name: 'topic',
        kind: 'decision',
        resolvers: resolvers((o) => (o === NONE ? 'No existing topic fits this document' : `Documents about ${o}`)),
        point: (c) => ({ question: 'Which existing topic does this document belong to?', options: [...step<string[]>(c, 'topics'), NONE], minConfidence: MIN_CONFIDENCE, evidence: text(c).slice(0, EXCERPT_CHARS) }),
      },
      { name: 'name', kind: 'code', run: propose },
      {
        name: 'file',
        kind: 'code',
        run: async (c) => {
          const topic = step<string>(c, 'name');
          const transcript: Transcript = {
            kind: 'remarkable.transcript',
            document: doc(c),
            type: step<Decision>(c, 'type').choice,
            topic,
            pages: step<string[]>(c, 'transcribe').map((t, i) => ({ page: i + 1, text: t })),
          };
          await must(c.api.env!.writeFile(transcriptKind.path, JSON.stringify(transcript, null, 1), c.ctx), transcriptKind.path);
          const sealed = await seal(c.api, transcriptKind, c.ctx);
          // Registered only once the transcript is sealed, so a failed ingest adds no topic; the service ignores a repeat.
          await deps.service.addTopic(topic);
          return sealed as unknown as JsonValue;
        },
      },
    ],
  });
}
