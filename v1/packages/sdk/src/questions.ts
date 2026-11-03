import type { Context } from '@earendil-works/chord/context';
import { defineDoc, type Conversation } from '@earendil-works/pi-durable';
import { whileWaiting } from './signals.ts';
import type { Resolver } from './decide.ts';

// Documents are strict JSON, so absent values are null rather than optional fields.
export type Question = {
  question: string;
  options: string[];
  evidence: string | null;
  // Absent in questions stored before these existed; readers treat undefined as null / {}.
  header?: string | null;
  title?: string | null;
  detail?: string | null;
  optionDescriptions?: Record<string, string>;
  suggested?: number | null;
  askedAt: number;
  answer: { choice: string; by: string; at: number } | null;
  /** The run that asked ended without an answer, so nobody is waiting on it. */
  closed: boolean;
};

type QuestionsDoc = { items: Record<string, Question> };

/** Pending human questions, beside Approvals: written by whichever extension escalates, served and indexed generically. */
export const Questions = defineDoc<QuestionsDoc>({
  kind: 'quack.questions',
  version: 1,
  scope: 'conversation',
  history: 'latest',
  fork: 'initial',
  initial: () => ({ items: {} }),
  checkpointWhen: () => false,
});

export type OpenQuestion = Question & { id: string };

export const pendingQuestions = (doc: Readonly<QuestionsDoc> | null | undefined): OpenQuestion[] =>
  Object.entries(doc?.items ?? {}).filter(([, q]) => !q.answer && !q.closed).map(([id, q]) => ({ id, ...q }));

export type AnswerResult = 'ok' | 'not-asking' | 'answered' | 'bad-choice';

/** Records a person's answer; the first answer stands. */
export async function answerQuestion(conversation: Conversation, id: string, choice: string, by: string, ctx: Context): Promise<AnswerResult> {
  return conversation.commit(async (tx): Promise<AnswerResult> => {
    const q = (await tx.doc(Questions, conversation.id)).items[id];
    if (!q || q.closed) return 'not-asking';
    if (q.answer) return 'answered';
    if (!q.options.includes(choice)) return 'bad-choice';
    q.answer = { choice, by, at: Date.now() };
    return 'ok';
  }, ctx);
}

/** After an abort nobody waits on the open questions any more. */
export async function closeQuestions(conversation: Conversation, ctx: Context): Promise<void> {
  await conversation.commit(async (tx) => {
    for (const q of Object.values((await tx.doc(Questions, conversation.id)).items)) if (!q.answer) q.closed = true;
  }, ctx);
}

// Last resolver of a decision chain: records the question and waits for a person. The id carries the task,
// so a rerun finds its question and a later run asks anew.
export function person(signal?: AbortSignal): Resolver {
  return {
    id: 'person',
    async resolve(point, api, ctx) {
      const id = `${String(api.taskId)}:${point.name}`;
      await api.commit(async (tx) => {
        const doc = await tx.doc(Questions, api.conversationId);
        doc.items[id] ??= { question: point.question, options: point.options, evidence: point.evidence ?? null, header: point.header ?? null, title: point.title ?? null, detail: point.detail ?? null, optionDescriptions: point.optionDescriptions ?? {}, suggested: point.suggested ?? null, askedAt: api.now(), answer: null, closed: false };
      }, ctx);
      const watch = await api.watchDoc(Questions, api.conversationId, ctx);
      if (!watch) throw new Error('question state is unavailable');
      const answer = await whileWaiting(() => new Promise<NonNullable<Question['answer']>>((resolve, reject) => {
        const stop = signal ?? ctx.abortSignal;
        const found = () => watch.value?.items[id]?.answer;
        const now = found();
        if (now) return resolve({ ...now });
        if (stop?.aborted) return reject(stop.reason);
        stop?.addEventListener('abort', () => reject(stop.reason), { once: true });
        watch.start(async () => { const a = found(); if (a) resolve({ ...a }); });
      }).finally(() => watch.stop()));
      return { choice: answer.choice, confidence: 1, reason: `answered by ${answer.by}` };
    },
  };
}
