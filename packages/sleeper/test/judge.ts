import { manifest } from '@quack/judge';

/** The judge as the sleeper agents enable it (one revision), installed before the workflow as their resolved order puts it. */
export const sleeperJudge = async () => manifest.session!({ maxRevisions: 1 }, {}, []);
