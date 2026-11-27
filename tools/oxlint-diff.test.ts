import assert from "node:assert/strict";
import { describe, it } from "node:test";
import { filterDiagnostics, parseDiffLines, type OxlintDiagnostic } from "./oxlint-diff.ts";

describe("oxlint-diff", () => {
  it("parses added and modified line ranges from unified -U0 diffs", () => {
    const diff = [
      "diff --git a/src/a.ts b/src/a.ts",
      "--- a/src/a.ts",
      "+++ b/src/a.ts",
      "@@ -10,0 +11,3 @@",
      "+const a = 1;",
      "+const b = 2;",
      "+const c = 3;",
      "@@ -20 +23 @@",
      "-old",
      "+new",
      "@@ -30,2 +33,0 @@",
      "-deleted1",
      "-deleted2",
    ].join("\n");
    const parsed = parseDiffLines(diff);
    assert.deepEqual([...(parsed.get("src/a.ts") ?? [])], [11, 12, 13, 23]);
  });

  it("keeps only diagnostics on touched lines", () => {
    const touched = new Map([["src/a.ts", new Set([12, 23])]]);
    const diags: OxlintDiagnostic[] = [
      { code: "no-unused-vars", severity: "error", message: "legacy", filename: "src/a.ts", labels: [{ span: { line: 5 } }] },
      { code: "no-unused-vars", severity: "error", message: "new", filename: "./src/a.ts", labels: [{ span: { line: 12, column: 7 } }] },
      { code: "no-explicit-any", severity: "error", message: "other file", filename: "src/b.ts", labels: [{ span: { line: 12 } }] },
    ];
    const hits = filterDiagnostics(diags, touched);
    assert.equal(hits.length, 1);
    assert.equal(hits[0]?.message, "new");
  });
});
