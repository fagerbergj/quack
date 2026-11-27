import { execFileSync } from "node:child_process";
import { pathToFileURL } from "node:url";

export interface OxlintDiagnostic {
  code: string;
  severity: string;
  message: string;
  filename: string;
  labels?: Array<{ span?: { line?: number; column?: number } }>;
}

const TS_JS_GLOBS = ["*.ts", "*.tsx", "*.js", "*.jsx", "*.mjs", "*.cjs"];
const OXLINT_PKG = "oxlint@1.95.0";

// Parse `git diff -U0` output into a map of repo-relative file path -> touched line numbers.
export function parseDiffLines(diffText: string): Map<string, Set<number>> {
  const byFile = new Map<string, Set<number>>();
  let current: Set<number> | undefined;
  for (const line of diffText.split("\n")) {
    if (line.startsWith("+++ b/")) {
      const file = line.slice("+++ b/".length);
      current = byFile.get(file) ?? new Set<number>();
      byFile.set(file, current);
      continue;
    }
    const hunk = /^@@ -\d+(?:,\d+)? \+(\d+)(?:,(\d+))? @@/.exec(line);
    if (!hunk || !current) continue;
    const start = Number(hunk[1]);
    const count = hunk[2] === undefined ? 1 : Number(hunk[2]);
    for (let i = 0; i < count; i++) current.add(start + i);
  }
  return byFile;
}

export function filterDiagnostics(
  diagnostics: readonly OxlintDiagnostic[],
  touched: ReadonlyMap<string, ReadonlySet<number>>,
): OxlintDiagnostic[] {
  return diagnostics.filter((d) => {
    const lines = touched.get(d.filename.replace(/^\.\//, ""));
    if (!lines) return false;
    const line = d.labels?.[0]?.span?.line;
    return line !== undefined && lines.has(line);
  });
}

function runOxlintJson(files: readonly string[]): OxlintDiagnostic[] {
  let stdout = "";
  try {
    stdout = execFileSync("npx", ["--yes", OXLINT_PKG, "--config", ".oxlintrc.json", "--format", "json", ...files], {
      encoding: "utf8",
      maxBuffer: 32 * 1024 * 1024,
    });
  } catch (err: unknown) {
    const out = (err as { stdout?: string | Buffer }).stdout;
    if (typeof out === "string" && out.trim().startsWith("{")) stdout = out;
    else if (Buffer.isBuffer(out) && out.toString("utf8").trim().startsWith("{")) stdout = out.toString("utf8");
    else throw err;
  }
  const parsed = JSON.parse(stdout) as { diagnostics?: OxlintDiagnostic[] };
  return parsed.diagnostics ?? [];
}

function main(baseRef: string): number {
  execFileSync("git", ["rev-parse", "--verify", baseRef], { stdio: "ignore" });
  const diffText = execFileSync(
    "git",
    ["diff", "-U0", "--diff-filter=ACMR", `${baseRef}...HEAD`, "--", ...TS_JS_GLOBS],
    { encoding: "utf8", maxBuffer: 32 * 1024 * 1024 },
  );
  const touched = parseDiffLines(diffText);
  const files = [...touched.entries()].filter(([, s]) => s.size > 0).map(([f]) => f);
  if (files.length === 0) return 0;

  const hits = filterDiagnostics(runOxlintJson(files), touched);
  for (const d of hits) {
    const loc = d.labels?.[0]?.span;
    const pos = loc?.line ? `:${loc.line}:${loc.column ?? 1}` : "";
    console.error(`${d.filename}${pos}: ${d.severity} [${d.code}] ${d.message}`);
  }
  return hits.length > 0 ? 1 : 0;
}

if (import.meta.url === pathToFileURL(process.argv[1] ?? "").href) {
  const base = process.argv[2] ?? "origin/main";
  process.exit(main(base));
}
