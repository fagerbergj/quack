import { expect, test } from 'vitest';
import { startEcho } from '../subjects/server.ts';

test('the echo server answers with the request path', async () => {
  const { url } = await startEcho();
  expect(await (await fetch(`${url}/a/b`)).text()).toBe('/a/b');
});
