#!/usr/bin/env node
// unwrap-md.mjs - join hard-wrapped prose paragraphs in markdown.
// Render-neutral: markdown treats a single newline inside a paragraph as a
// space, so joining wrapped lines changes the diff, not the page.
//
// Only joins "prose" lines: non-empty, <100 chars, no " | " (table-shaped;
// a pipe at the line edge alone doesn't qualify), not a list item / heading
// / table row / blockquote / fence / setext underline, and not ending in a
// hard break (two trailing spaces or a backslash).
//
// Usage: node scripts/unwrap-md.mjs [--check] <paths...>
//   default: rewrite in place, print files changed
//   --check: report hard-wrapped files, exit 1 if any (CI mode)
import { readFileSync, writeFileSync } from "node:fs";

const check = process.argv.includes("--check");
const args = process.argv.slice(2).filter((a) => a !== "--check");

// A line is joinable prose if it could be the middle of a hard-wrapped
// paragraph. The <100 char cap means already-unwrapped (long) lines never
// get absorbed. " | " (space-pipe-space) marks table-shaped lines, including
// pipe-less tables whose rows have no leading pipe; a lone pipe at the line
// edge or an inline enum like `a | b | c` in prose stays joinable, except
// the leading-pipe table row the exclusion list below already rejects.
const PROSE = (l) =>
  l.trim().length > 0 &&
  l.trim().length < 100 &&
  !l.includes(" | ") &&
  !/^\s*\|/.test(l) &&
  !/^\s*(#|>|\||-|\*|\+|\d+[.)])/.test(l) && // heading, quote, table, list
  !/^ {2,}\S/.test(l) && // indented code or nested list
  !/^\s*([-*_=]){3,}\s*$/.test(l) && // setext underline / hr
  !/\s{2}$/.test(l) && // deliberate <br>
  !/\\$/.test(l); // hard break

function unwrap(text) {
  const out = [];
  let canJoin = false;
  let fence = null;
  let joins = 0;
  for (const raw of text.split("\n")) {
    if (/^\s*(```|~~~)/.test(raw)) {
      fence = fence ? null : raw.match(/^\s*(```|~~~)/)[1];
      out.push(raw);
      canJoin = false;
      continue;
    }
    if (fence || !PROSE(raw)) {
      out.push(raw);
      canJoin = false;
      continue;
    }
    if (canJoin) {
      out[out.length - 1] = out[out.length - 1].replace(/\s+$/, "") + " " + raw.trim();
      joins++;
      canJoin = true; // paragraph continues
    } else {
      out.push(raw);
      canJoin = true;
    }
  }
  return { text: out.join("\n"), joins };
}

const findings = [];
for (const p of args) {
  const text = readFileSync(p, "utf8");
  const { text: next, joins } = unwrap(text);
  if (joins > 0) findings.push([p, joins]);
  if (!check && next !== text) writeFileSync(p, next);
}
if (findings.length) {
  for (const [p, n] of findings) console.log(check ? `hard-wrapped: ${p} (${n} joins)` : `unwrapped: ${p} (${n} joins)`);
  if (check) process.exit(1);
} else {
  console.log(check ? "unwrap-md: ok" : "unwrap-md: nothing to do");
}
