## Takeaway
One sentence.

## Verdict
approve

## Findings
- blocking (defect): src/a.ts:10 — the guard is inverted.
```diff
- if (ok) return;
+ if (!ok) return;
- nit: this is code, not a finding
```
- nit: src/b.ts:2 — rename it.
  ```ts
  const x = 1;
  ```
