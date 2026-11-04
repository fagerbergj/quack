import { readDeploymentFile, serviceDbPath, serviceEntry, serviceToken } from '@quack/sdk';
import { slotLimits } from './limits.ts';
import { createSlotsService } from './service.ts';

const file = readDeploymentFile();
const service = createSlotsService({
  serviceToken: serviceToken(serviceEntry(file, 'slots'), 'slots'),
  dbPath: serviceDbPath(file, 'slots'),
  limits: slotLimits({ providers: file.providers ?? {} }),
});
service.listen(Number(process.env.PORT || 8092));
for (const signal of ['SIGTERM', 'SIGINT'] as const) process.on(signal, () => service.close(() => process.exit(0)));
