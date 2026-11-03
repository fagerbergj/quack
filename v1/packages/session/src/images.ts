import { mkdir, readFile, writeFile } from 'node:fs/promises';
import { join } from 'node:path';
import { contentText, type ImageContent, type Message, type MutableModels } from '@earendil-works/pi-ai';
import { obj, str } from '@earendil-works/pi-schema';
import { defineExtension, defineTool } from '@earendil-works/pi-durable';
import { observeGeneration } from '@quack/sdk';

/** Per message; the browser downscales to a 2048 px long edge first, so a photo is well under the byte cap. */
export const MAX_IMAGES = 4;
export const MAX_IMAGE_CHARS = 3 * 1024 * 1024;
const EXT: Record<string, string> = { 'image/png': 'png', 'image/jpeg': 'jpg', 'image/webp': 'webp' };
const MIME = Object.fromEntries(Object.entries(EXT).map(([m, e]) => [e, m]));
const DIR = 'attachments';

export const NO_IMAGE_MODEL = "Images need an image model: this agent's model cannot see them and the deployment names no imageModel.";

/** The images of an `input` frame, or the problem with them. */
export function parseImages(value: unknown): ImageContent[] | string {
  if (value === undefined) return [];
  if (!Array.isArray(value)) return 'images must be a list';
  if (value.length > MAX_IMAGES) return `at most ${MAX_IMAGES} images per message`;
  for (const i of value) {
    if (typeof i !== 'object' || i === null || typeof i.data !== 'string' || !(i.mimeType in EXT)) return 'each image needs base64 data and a png, jpeg or webp mimeType';
    if (i.data.length > MAX_IMAGE_CHARS) return `an image is over ${MAX_IMAGE_CHARS >> 20} MB`;
  }
  return value.map((i) => ({ type: 'image', data: i.data, mimeType: i.mimeType }));
}

/** Saves the images in the session's workspace and returns the paths the agent can name. */
export async function saveImages(workspace: string, images: ImageContent[]): Promise<string[]> {
  await mkdir(join(workspace, DIR), { recursive: true });
  const stamp = Date.now();
  return Promise.all(images.map(async (img, i) => {
    const path = `${DIR}/${stamp}-${i + 1}.${EXT[img.mimeType]}`;
    await writeFile(join(workspace, path), Buffer.from(img.data, 'base64'));
    return path;
  }));
}

const fail = (text: string) => ({ output: [{ type: 'text' as const, text }], isError: true });

/** For an agent whose model cannot see images: asks the deployment's image model about one saved attachment. */
export function lookExtension(workspace: string, models: MutableModels, imageModel: { provider: string; modelId: string }) {
  return defineExtension({
    name: 'attachments',
    tools: [defineTool({
      name: 'look_at_image',
      description: 'Ask an image-capable model a question about an attached image, such as "what is in it" or "read the text". You cannot see images yourself.',
      parameters: obj({ path: str({ description: `The attachment path, such as ${DIR}/1-1.jpg` }), question: str({ minLength: 1 }) }, { open: true }),
      replay: 'safe',
      async execute(args, api, ctx) {
        const [dir, file] = args.path.split('/');
        const mimeType = MIME[file?.split('.').pop() ?? ''];
        if (dir !== DIR || !mimeType || !/^[\w.-]+$/.test(file)) return fail(`path must be an attachment path such as ${DIR}/1-1.jpg`);
        const data = await readFile(join(workspace, DIR, file), 'base64').catch(() => undefined);
        const model = models.getModel(imageModel.provider, imageModel.modelId);
        if (data === undefined || !model) return fail(data === undefined ? `no attachment ${args.path}` : 'the image model is not available');
        const messages: Message[] = [{ role: 'user', content: [{ type: 'text', text: args.question }, { type: 'image', data, mimeType }], timestamp: api.now() }];
        const reply = await observeGeneration({ provider: model.provider, signal: ctx.abortSignal, messages }, (r) => models.completeSimple(model, r, { signal: ctx.abortSignal }));
        return reply.stopReason === 'error' ? fail(`the image model failed: ${reply.errorMessage}`) : { output: [{ type: 'text' as const, text: contentText(reply.content) }] };
      },
    })],
  });
}
