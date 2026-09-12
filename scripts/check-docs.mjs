#!/usr/bin/env node
// check-docs.mjs - documentation drift gate. Three checks, zero deps:
//   1. local markdown link targets must exist (docs/**, root *.md)
//   2. backticked repo path refs (internal/ cmd/ frontend/src/ ...) must exist on disk
//   3. every yaml:"key" tag in internal/config/*.go must appear in docs/ or config/quack.yaml
// Exit 1 with a finding list. Run via `make docs-check`.
import { readFileSync, existsSync, readdirSync, statSync } from "node:fs";
import { join, dirname } from "node:path";
import { fileURLToPath } from "node:url";

const root = join(dirname(fileURLToPath(import.meta.url)), "..");
const findings = [];

function mdFiles(dir, out = []) {
  for (const e of readdirSync(dir)) {
    const p = join(dir, e);
    const st = statSync(p);
    if (st.isDirectory()) mdFiles(p, out);
    else if (e.endsWith(".md")) out.push(p);
  }
  return out;
}

// --- 1+2: links and path refs -------------------------------------------
const docs = [
  ...mdFiles(join(root, "docs")),
  ...readdirSync(root)
    .filter((f) => f.endsWith(".md"))
    .map((f) => join(root, f)),
];

const LINK_RE = /\[[^\]]*\]\(([^)\s]+)\)/g;
const PATH_RE = /`((?:internal|cmd|frontend\/src|tools|scripts|agents|skills|deploy)\/[\w./-]+)`/g;

for (const file of docs) {
  const text = readFileSync(file, "utf8");
  for (const m of text.matchAll(LINK_RE)) {
    let target = m[1];
    if (/^(https?:|mailto:|#)/.test(target)) continue;
    target = target.split("#")[0];
    if (!target) continue;
    const base = join(dirname(file), target);
    if (!existsSync(base) && !existsSync(base + ".md") && !existsSync(join(base, "index.md"))) {
      findings.push(`${rel(file)}: broken link -> ${m[1]}`);
    }
  }
  for (const m of text.matchAll(PATH_RE)) {
    const p = m[1];
    if (p.includes("${") || p.includes("*") || p.includes("<")) continue;
    // a capitalized second segment is a Go symbol (pkg.Func), not a path;
    // so is a dotted filename whose "extension" isn't a real file type
    const seg = p.split("/")[1] ?? "";
    if (/^[A-Z]/.test(seg)) continue;
    const last = p.split("/").pop();
    if (/\./.test(last) && !/\.(go|md|mjs|js|ts|tsx|json|ya?ml|sh|sql|html|pem|zip)$/.test(last)) continue;
    const t = p.replace(/\/+$/, "");
    if (!t || !existsSync(join(root, t))) {
      findings.push(`${rel(file)}: no such path -> \`${p}\``);
    }
  }
}

// --- 3: blank line before blockquotes / around standalone hr -----------
// (format-markdown rule that no markdownlint rule covers; a setext
// underline is the previous line being text, not a blank - left to MD003)
for (const file of docs) {
  const lines = readFileSync(file, "utf8").split("\n");
  const blank = (i) => i < 0 || i >= lines.length || lines[i].trim() === "";
  for (let i = 0; i < lines.length; i++) {
    const l = lines[i];
    if (/^\s*>/.test(l)) {
      if (!blank(i - 1) && !/^\s*>/.test(lines[i - 1]))
        findings.push(`${rel(file)}:${i + 1}: blockquote not preceded by a blank line`);
      if (!blank(i + 1) && !/^\s*>/.test(lines[i + 1]))
        findings.push(`${rel(file)}:${i + 1}: blockquote not followed by a blank line`);
    }
    if (/^\s*(?:-{3,}|\*{3,}|_{3,})\s*$/.test(l) && blank(i - 1)) {
      if (!blank(i + 1))
        findings.push(`${rel(file)}:${i + 1}: horizontal rule not followed by a blank line`);
    }
  }
}

// --- 4: config keys must be documented ----------------------------------
const cfgDir = join(root, "internal", "config");
const yamlTagRe = /yaml:"([a-z_]+)(?:,|")/g;
const keys = new Set();
for (const f of readdirSync(cfgDir).filter((f) => f.endsWith(".go") && !f.endsWith("_test.go"))) {
  for (const m of readFileSync(join(cfgDir, f), "utf8").matchAll(yamlTagRe)) keys.add(m[1]);
}
let docsCorpus = "";
for (const f of mdFiles(join(root, "docs"))) docsCorpus += readFileSync(f, "utf8");
const shippedYaml = readFileSync(join(root, "config", "quack.yaml"), "utf8");

for (const k of keys) {
  const re = new RegExp(`\\b${k}\\b`);
  if (!re.test(docsCorpus) && !re.test(shippedYaml)) {
    findings.push(`config key \`${k}\` (internal/config) is in neither docs/ nor config/quack.yaml`);
  }
}

function rel(p) {
  return p.slice(root.length + 1);
}

if (findings.length) {
  console.error(`check-docs: ${findings.length} finding(s):`);
  for (const f of findings) console.error("  " + f);
  process.exit(1);
}
console.log(`check-docs: ok (${docs.length} files, ${keys.size} config keys)`);
