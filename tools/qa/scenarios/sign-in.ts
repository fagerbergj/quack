import assert from 'node:assert/strict';
import { createServer, request, type Server } from 'node:http';
import type { Socket } from 'node:net';
import { eachView, visible, type Scenario } from '../scenario.ts';

// The API paths a forward-auth proxy redirects once the sign-in expires; the page itself loads, as when the login bounces straight back.
const API = /^\/(sessions|version|models|settings|agents|apps|artifacts|asks|push)/;

const listen = (server: Server) => new Promise<number>((ok) => server.listen(0, '127.0.0.1', () => ok((server.address() as { port: number }).port)));

/** A proxy on another host name in front of the stack that, once `expire()` is called, 302s API calls to a login on another origin and drops open streams. */
async function forwardAuth(stack: string) {
  const up = new URL(stack);
  let expired = false;
  const sockets = new Set<Socket>();
  const login = createServer((_q, s) => void s.writeHead(200, { 'content-type': 'text/html' }).end('<h1>login</h1>'));
  const loginUrl = `http://127.0.0.1:${await listen(login)}/flow`;
  const front = createServer((q, s) => {
    if (expired && API.test(q.url ?? '/')) return void s.writeHead(302, { location: loginUrl }).end();
    q.pipe(request({ host: up.hostname, port: up.port, path: q.url, method: q.method, headers: q.headers }, (r) => (s.writeHead(r.statusCode!, r.headers), r.pipe(s))));
  });
  front.on('connection', (c) => (sockets.add(c), c.on('close', () => sockets.delete(c))));
  const url = `http://localhost:${await listen(front)}`;
  return {
    url,
    expire: () => ((expired = true), sockets.forEach((c) => c.destroy())),
    reset: () => void (expired = false),
    close: () => (front.closeAllConnections(), front.close(), login.close()),
  };
}

// With the sign-in expired and the login bouncing straight back, a dropped stream reloads the page once, then the banner asks for the reload.
const scenario: Scenario = {
  name: 'sign-in',
  stack: {},
  async run(ctx) {
    const proxy = await forwardAuth(ctx.url);
    try {
      await eachView(ctx, 'expired sign-in banner', async (page, label) => {
        proxy.reset();
        await page.goto(`${proxy.url}/#/sessions`);
        await visible(page.getByRole('searchbox').or(page.getByPlaceholder('Search sessions')).first());
        let loads = 0;
        page.on('load', () => loads++);
        proxy.expire();
        const banner = page.getByRole('alert').filter({ has: page.getByRole('button', { name: 'Reload' }) });
        await visible(banner);
        assert.equal(loads, 1, 'one automatic reload, then the banner');
        const bottom = (await banner.boundingBox())!.y + (await banner.boundingBox())!.height;
        const main = (await page.locator('#main').boundingBox())!;
        assert.ok(main.y >= bottom - 1, `the page starts below the banner (${main.y} < ${bottom})`);
        const header = page.getByRole('banner');
        if (await header.isVisible()) assert.ok((await header.boundingBox())!.y >= bottom - 1, 'the header starts below the banner');
        await ctx.shot(page, `sign-in-banner-${label}`);
        await page.waitForTimeout(2000);
        assert.equal(loads, 1, 'no reload loop');
      });
    } finally {
      proxy.close();
    }
  },
};
export default scenario;
