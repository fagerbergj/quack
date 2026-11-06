import type { ImageContent } from '@earendil-works/pi-ai';

/** Per message; the browser downscales to a 2048 px long edge first, so a photo is well under the byte cap. */
export const MAX_IMAGES = 4;
export const MAX_IMAGE_CHARS = 3 * 1024 * 1024;
const EXT: Record<string, string> = { 'image/png': 'png', 'image/jpeg': 'jpg', 'image/webp': 'webp' };

/** Vision comes from the chosen model's input modalities; there is no deployment-wide fallback model. */
export const CANNOT_SEE_IMAGES = "This agent's model cannot see images; choose a model that takes image input.";

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
