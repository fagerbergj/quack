import { createServer } from 'node:http';

// Spawned by tests: listens on PORT after a startup delay, like a real service loading its config.
setTimeout(() => {
  createServer((_req, res) => res.end('ok')).listen(Number(process.env.PORT), '127.0.0.1');
}, 50);
