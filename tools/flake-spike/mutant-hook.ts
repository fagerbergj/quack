// Preloaded by detector D: serves FLAKE_MUTANT_SRC's text in place of FLAKE_MUTANT_FILE for this one node:test run.
import { readFileSync } from 'node:fs';
import { registerHooks } from 'node:module';
import { resolve } from 'node:path';
import { pathToFileURL } from 'node:url';

const file = process.env.FLAKE_MUTANT_FILE;
const target = file && pathToFileURL(resolve(file)).href;

if (target) {
  registerHooks({
    load(url, context, next) {
      if (url !== target) return next(url, context);
      return { format: 'module-typescript', source: readFileSync(process.env.FLAKE_MUTANT_SRC!, 'utf8'), shortCircuit: true };
    },
  });
}
