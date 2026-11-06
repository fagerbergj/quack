// Runs at npm install: lets an unquoted liqe term carry Lucene backslash escapes (key:sleeper\:platinum\:*).
// shortcut: a local grammar patch; send it upstream to gajus/liqe and drop this once a release carries escapes.
import { readFileSync, writeFileSync } from 'node:fs';
import { createRequire } from 'node:module';

const VERSION = '3.8.8';
const require = createRequire(import.meta.url);
const pkg = JSON.parse(readFileSync(require.resolve('liqe/package.json'), 'utf8')) as { version: string };
if (pkg.version !== VERSION) throw new Error(`tools/patch-liqe.ts patches liqe ${VERSION}, found ${pkg.version}: re-check the grammar edit`);
const file = require.resolve('liqe/dist/src/grammar.js');
let text = readFileSync(file, 'utf8');

// The grammar spells its class bound as the six characters \uFFFF.
const HI = String.raw`\u0080-\u` + 'FFFF';
const FIRST = String.raw`/[#$*?@A-Z_a-z${HI}]/`;
const REST = String.raw`/[\w#$*.?@${HI}\-]/`;
const MID = String.raw`/[#$*?@A-Z_a-z${HI}\-]/`;
const edits: Array<[from: string, to: string]> = [
  [`symbols: ['unquoted_value$ebnf$1', ${REST}]`, `symbols: ['unquoted_value$ebnf$1', 'uq_rest']`],
  [`symbols: [${FIRST}, 'unquoted_value$ebnf$1']`, `symbols: ['uq_first', 'unquoted_value$ebnf$1']`],
  [`symbols: ['unquoted_value$ebnf$3', ${REST}]`, `symbols: ['unquoted_value$ebnf$3', 'uq_rest']`],
  [`'unquoted_value$ebnf$2',\n                ${MID},`, `'unquoted_value$ebnf$2',\n                'uq_mid',`],
  ["    ],\n    ParserStart: 'main',", [
    String.raw`        { name: 'uq_escape', postprocess: (d) => d.join(''), symbols: [{ literal: '\\' }, /[\s\S]/] },`,
    ...[['uq_first', FIRST], ['uq_rest', REST], ['uq_mid', MID]].flatMap(([name, cls]) => [
      `        { name: '${name}', postprocess: (d) => d[0], symbols: [${cls}] },`,
      `        { name: '${name}', postprocess: (d) => d[0], symbols: ['uq_escape'] },`,
    ]),
    "    ],\n    ParserStart: 'main',",
  ].join('\n')],
];

if (!text.includes("name: 'uq_escape'")) {
  for (const [from, to] of edits) {
    if (text.split(from).length !== 2) throw new Error(`tools/patch-liqe.ts: expected exactly one ${JSON.stringify(from)} in ${file}`);
    text = text.replace(from, () => to);
  }
  writeFileSync(file, text);
}
