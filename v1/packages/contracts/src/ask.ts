import { arr, obj, str } from '@earendil-works/pi-schema';
import { defineTool } from '@earendil-works/pi-durable';
import { decide, person } from '@quack/sdk';

export const YOU_DECIDE = 'You decide';

const params = obj({
  id: str({ pattern: '^[a-z][a-z0-9-]*$', description: 'Stable slug for this question, e.g. "pagination"; a rerun finds the same question by it' }),
  question: str({ minLength: 1, description: 'One question in plain words' }),
  header: str({ minLength: 1, maxLength: 12, description: 'A chip of at most 12 characters naming the topic, e.g. "Errors"' }),
  title: str({ minLength: 1, maxLength: 100, description: 'The question as a headline, at most about 80 characters; `code` allowed' }),
  detail: str({ minLength: 1, description: 'One to three sentences of Markdown context for the person' }),
  matters: str({ minLength: 1, description: 'One sentence: why the answer matters to this contract' }),
  suggested: str({ minLength: 1, maxLength: 80, description: 'Your recommended answer as a short label, shown first' }),
  suggestedTradeoff: str({ minLength: 1, description: 'One line: what choosing it costs or gives' }),
  alternatives: arr(obj({ answer: str({ minLength: 1, maxLength: 80, description: 'Short label' }), tradeoff: str({ minLength: 1 }) }, { open: true }), { description: 'Other reasonable answers, each with its trade-off' }),
}, { open: true });

/** Asks the person one question: the suggested answer first, then alternatives, "You decide" last. */
export const askQuestionTool = defineTool({
  name: 'ask_question',
  description: 'Ask the person one open question about the contract and wait for the answer. Ask one at a time, only what the code and the guide cannot settle. Returns the answer to cite in a choice\'s why.',
  parameters: params,
  replay: 'safe',
  async execute({ id, question, header, title, detail, matters, suggested, suggestedTradeoff, alternatives }, api, ctx) {
    const options = [suggested, ...alternatives.map((a) => a.answer).filter((a) => a !== suggested), YOU_DECIDE];
    const trade = alternatives.map((a) => `- ${a.answer}: ${a.tradeoff}`).join('\n');
    const evidence = `${matters}\n\nSuggested: ${suggested}${trade ? `\n\nAlternatives:\n${trade}` : ''}`;
    const optionDescriptions = Object.fromEntries([[suggested, suggestedTradeoff], ...alternatives.map((a) => [a.answer, a.tradeoff])].filter((e): e is [string, string] => !!e[1]));
    const d = await decide(api, { name: id, question, options, evidence, header, title, detail, optionDescriptions, suggested: 0 }, [person(ctx.abortSignal)], ctx);
    if (!d) return { output: [{ type: 'text', text: 'Nobody answered.' }], isError: true };
    const answer = d.choice === YOU_DECIDE ? `${YOU_DECIDE}: use the suggested "${suggested}" and say in the why that the person delegated it` : d.choice;
    return { output: [{ type: 'text', text: `Answer: ${answer}\nCite it in a why with source "answer", ref "${question}" (the question exactly as asked) and text "${answer}".` }] };
  },
});
