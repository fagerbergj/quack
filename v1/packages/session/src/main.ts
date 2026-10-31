import { createModels, createProvider, envApiKeyAuth } from '@earendil-works/pi-ai';
import { openAICompletionsApi } from '@earendil-works/pi-ai/api/openai-completions.lazy';
import { openSession } from './runtime.ts';
import { createSessionServer } from './server.ts';

function env(name: string, fallback?: string): string {
  const value = process.env[name] ?? fallback;
  if (value === undefined) throw new Error(`${name} is required`);
  return value;
}

// shortcut: one OpenAI-compatible provider from env; agent definitions from the distributor replace this in a later slice.
const modelId = env('QUACK_MODEL');
const baseUrl = env('QUACK_MODEL_BASE_URL');
const models = createModels();
models.setProvider(
  createProvider({
    id: 'local',
    baseUrl,
    auth: { apiKey: envApiKeyAuth('local', ['QUACK_MODEL_API_KEY']) },
    models: [{ id: modelId, name: modelId, api: 'openai-completions', provider: 'local', baseUrl, reasoning: true, input: ['text'], cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0 }, contextWindow: Number(env('QUACK_MODEL_CONTEXT', '131072')), maxTokens: 16384 }] as never,
    api: openAICompletionsApi(),
  }),
);

const session = await openSession({ dir: env('QUACK_SESSION_DIR'), models, agent: { model: { provider: 'local', modelId } } });
const server = createSessionServer(session);
server.listen(Number(env('PORT', '8080')));

for (const signal of ['SIGTERM', 'SIGINT'] as const) {
  process.on(signal, () => server.close(() => void session.close().then(() => process.exit(0))));
}
