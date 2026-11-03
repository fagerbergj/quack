import { arr, obj, str } from '@earendil-works/pi-schema';
import { defineTool } from '@earendil-works/pi-durable';
import { decide, person } from '@quack/sdk';

export const YOU_DECIDE = 'You decide';

const params = obj({
  id: str({ pattern: '^[a-z][a-z0-9-]*$', description: 'Stable slug for this question, e.g. "pagination"; a rerun finds the same question by it' }),
  question: str({ minLength: 1, description: 'One question in plain words' }),
  matters: str({ minLength: 1, description: 'One sentence: why the answer matters to this contract' }),
  suggested: str({ minLength: 1, description: 'Your recommended answer, pre-selected for the person' }),
  alternatives: arr(obj({ answer: str({ minLength: 1 }), tradeoff: str({ minLength: 1 }) }, { open: true }), { description: 'Other reasonable answers, each with its trade-off' }),
}, { open: true });

/** Asks the person one question: the suggested answer first, then alternatives, "You decide" last. */
export const askQuestionTool = defineTool({
  name: 'ask_question',
  description: 'Ask the person one open question about the contract and wait for the answer. Ask one at a time, only what the code and the guide cannot settle. Returns the answer to cite in a choice\'s why.',
  parameters: params,
  replay: 'safe',
  async execute({ id, question, matters, suggested, alternatives }, api, ctx) {
    const options = [suggested, ...alternatives.map((a) => a.answer).filter((a) => a !== suggested), YOU_DECIDE];
    const trade = alternatives.map((a) => `- ${a.answer}: ${a.tradeoff}`).join('\n');
    const evidence = `${matters}\n\nSuggested: ${suggested}${trade ? `\n\nAlternatives:\n${trade}` : ''}`;
    const d = await decide(api, { name: id, question, options, evidence }, [person(ctx.abortSignal)], ctx);
    if (!d) return { output: [{ type: 'text', text: 'Nobody answered.' }], isError: true };
    const answer = d.choice === YOU_DECIDE ? `${YOU_DECIDE}: use the suggested "${suggested}" and say in the why that the person delegated it` : d.choice;
    return { output: [{ type: 'text', text: `Answer: ${answer}\nCite it in a why with source "answer", ref "${question}" (the question exactly as asked) and text "${answer}".` }] };
  },
});
