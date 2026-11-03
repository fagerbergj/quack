import { int, lit, obj, str, union } from '@earendil-works/pi-schema';
import { defineTool } from '@earendil-works/pi-durable';
import type { SessionConfig } from './checkout.ts';

/** Answers a mention in its thread through the service, which strips mentions and holds the token. */
export function replyTool(config: SessionConfig) {
  return defineTool({
    name: 'reply_mention',
    description: 'Answer a PR comment that mentioned you, in its own thread. Pass the arguments the message gave you.',
    parameters: obj({
      repository: str({ description: 'owner/repo' }),
      number: int({ minimum: 1 }),
      kind: union([lit('review'), lit('issue')]),
      commentId: int({ minimum: 1 }),
      body: str({ minLength: 1 }),
    }, { open: true }),
    async execute(args) {
      const [owner, repo] = args.repository.split('/');
      if (!owner || !repo) return { output: [{ type: 'text', text: 'repository must be owner/repo' }], isError: true };
      const res = await fetch(`${config.serviceUrl}/replies`, { method: 'POST', body: JSON.stringify({ owner, repo, number: args.number, kind: args.kind, commentId: args.commentId, body: args.body }), headers: { authorization: `Bearer ${config.serviceToken}` } });
      if (!res.ok) return { output: [{ type: 'text', text: `Reply failed: ${res.status} ${(await res.text()).slice(0, 300)}` }], isError: true };
      return { output: [{ type: 'text', text: `Replied: ${((await res.json()) as { url?: string }).url ?? 'posted'}` }] };
    },
  });
}
