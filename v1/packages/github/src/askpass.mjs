#!/usr/bin/env node
// git's askpass for github.com: answers the username and fetches a fresh installation token; nothing is stored.
const prompt = process.argv[2] ?? '';
if (/^Username/i.test(prompt)) {
  process.stdout.write('x-access-token\n');
} else {
  const url = `${process.env.QUACK_GH_SERVICE_URL}/git-credential?url=${encodeURIComponent(process.env.QUACK_GH_REMOTE ?? '')}`;
  const res = await fetch(url, { headers: { authorization: `Bearer ${process.env.QUACK_GH_SERVICE_TOKEN}` } });
  if (!res.ok) process.exit(1);
  process.stdout.write(`${(await res.json()).token}\n`);
}
