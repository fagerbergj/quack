## Takeaway
Nothing moved since my last review: the head is the same commit, and the blocking `requestId` race, the open contract question, and the writer-duplication nit all stand as posted.

## Verdict
request_changes

## Findings
- blocking (defect): packages/remarkable/src/store.ts:21 — Still open: no reply or change since the finding was posted; store.ts:21 and the `store.batchFor` lookup at service.ts:150 are unchanged in this diff. Two in-flight POSTs /ingest with the same `requestId` both miss the lookup (it is separated from the insert by the `documents()` fetch), so both reach `store.create`; the second `INSERT INTO batch_requests` hits the PRIMARY KEY after its `INSERT INTO batches` already committed, so that request answers 500 instead of the original batch and leaves an orphaned row in the never-pruned `batches` table. That is exactly the duplicate idempotency keys exist to absorb (a tablet that times out and resends while the first request is still processing), and the endpoint is credential-free per the manifest pages, so any client can hit it. Claim the key atomically before creating anything:

  ```ts
  create(items, request?: string): string {
    const id = randomUUID();
    if (request !== undefined) {
      // Insert the claim first: a losing concurrent duplicate gets the winner's batch, not a 500.
      db.prepare('INSERT OR IGNORE INTO batch_requests (request, batch) VALUES (?, ?)').run(request, id);
      const held = db.prepare('SELECT batch FROM batch_requests WHERE request = ?').get(request) as { batch: string };
      if (held.batch !== id) return held.batch;
    }
    db.prepare('INSERT INTO batches (id, created) VALUES (?, ?)').run(id, now().toISOString());
    // ...batch_items as before
  }
  ```

  (The batch row is inserted only if the key insert actually claimed the key.) A test submitting two concurrent requests with the same key would pin this.
- question: packages/remarkable/src/service.ts:191 — Still open: no reply since the question was posted; `route` still answers 404 without an `Allow` header for a wrong method on a known path other than `/topics` (e.g. `GET /ingest`), while the description lists 405 with `Allow` only for `/topics`. The risk: a client that distinguishes the two will read 404 as "no such resource" and stop trying, instead of learning the method set from `Allow`. Was `GET /ingest` → 404 checked against contract v5, or should it be 405 with `Allow: POST`?
- nit: packages/remarkable/src/service.ts:46 — Still open: `reply` still re-serializes the problem shape (`type: 'about:blank'`, `title: STATUS_CODES[status]`, `detail`, `error`) by hand, a second writer for the same contract as `sendProblem`; if the problem shape ever changes, the two can drift. An `extra` parameter on the shared writer would let the 404-with-`ids` path use it too:

  ```ts
  export function sendProblem(res: ServerResponse, status: number, detail: string, extra?: object): void {
    res.writeHead(status, { 'content-type': 'application/problem+json' })
      .end(JSON.stringify({ type: 'about:blank', title: STATUS_CODES[status] ?? 'Error', status, detail, error: detail, ...extra }));
  }
  ```

## Verified
- The head is the exact commit (47b11c78) my review was posted against: the diff against origin/main is the identical 9-file change, and every commented line (service.ts:46, service.ts:191, store.ts:21) is unchanged in it.
- In that review I read the full diff plus the surrounding code (`service.ts`, `store.ts`, `ingest/index.ts`, the web `api.ts` readers, the distributor gateway `apps.ts`, the manifest pages, the `schedules` sibling), ran the remarkable package suite on this head (49/49 pass, including every new test), and confirmed `npm run typecheck` exits 0 and `npm run sloplint -- diff origin/main` is ok.
- Verified the SQLite pagination with a scratch DB on node:sqlite (`CREATE TABLE topics (name TEXT PRIMARY KEY COLLATE NOCASE)`): `WHERE name > ?`, `ORDER BY name` and `=` are all case-insensitive and return the stored spelling, so tokens built from stored spellings page without gaps or duplicates.
- CI on this head is green: `check=success`, `isolation=success`, `image=skipped`.
- The QA rig (`tools/qa/run.ts --scenario remarkable`) still cannot run in this sandbox (no chromium system libraries, not root); its HTTP assertions are covered by the unit tests I ran, and the 401 assertion exercises pre-existing gateway behaviour. The claim rests on the description's evidence.

## Notes
- `main` has moved since the first pass, but the PR's merge-base diff is unchanged, so none of the findings are superseded.
- Contract `69e5aecf…` v5 is not in the repo, so field-level fidelity is checked against the description's enumeration rather than the contract text.
- The test coverage is a strength: the requestId test surviving a restart, the paging tests walking pageSize/token edges, and the 405 `Allow` assertion all test the contract's actual words, not just happy paths.
