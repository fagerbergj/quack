// A session process whose fake model echoes the last user message, for distributor tests.
import { createModels, fauxAssistantMessage, fauxProvider } from '@earendil-works/pi-ai';
import { createSessionServer, openSession } from '@quack/session';

const faux = fauxProvider({ models: [{ id: 'echo' }] });
faux.setResponses(
  Array.from({ length: 50 }, () => (context: { messages: Array<{ content: unknown }> }) => {
    const content = context.messages.at(-1)?.content;
    const text = typeof content === 'string' ? content : (content as Array<{ text?: string }>).map((c) => c.text ?? '').join('');
    return fauxAssistantMessage(text === 'whoami' ? `agent: ${process.env.QUACK_AGENT}` : text === 'secret' ? `secret: ${process.env.PLANTED_SECRET ?? 'none'}` : `echo: ${text}`);
  }) as never,
);
const models = createModels();
models.setProvider(faux.provider);
const m = faux.getModel();

const session = await openSession({ dir: process.env.QUACK_SESSION_DIR!, models, agent: { model: { provider: m.provider, modelId: m.id } } });
const server = createSessionServer(session).listen(Number(process.env.PORT));
process.on('SIGTERM', () => server.close(() => void session.close().then(() => process.exit(0))));
