import { useEffect, useState } from 'react';
import { listArtifacts, type SealedArtifact } from './api.ts';

// Checks that did not fully hold; a score under the threshold can fail a version with none listed.
const failing = (a: SealedArtifact) => (a.verdict?.checks ?? []).filter((c) => c.met !== null && c.met < 1).map((c) => `${c.criterion}.${c.id}`);

function Item({ a }: { a: SealedArtifact }) {
  const status = a.verdict ? (a.verdict.passed ? 'passed' : 'failed') : 'not checked';
  const failed = failing(a);
  return (
    <li>
      <details>
        <summary>{`${a.kind} v${a.version}: ${status}`}</summary>
        {failed.length > 0 && <p>{`Failing: ${failed.join(', ')}`}</p>}
        <pre>{a.content}</pre>
      </details>
    </li>
  );
}

// `refresh` changes whenever the session saw tool activity, which is when a version can be sealed or checked.
export function Artifacts({ sessionKey, refresh }: { sessionKey: string; refresh: number }) {
  const [items, setItems] = useState<SealedArtifact[]>([]);
  const [error, setError] = useState('');
  useEffect(() => void listArtifacts(sessionKey).then(setItems, (e: Error) => setError(e.message)), [sessionKey, refresh]);
  if (!items.length && !error) return null;
  return (
    <section aria-labelledby="artifacts-h">
      <h2 id="artifacts-h">Artifacts</h2>
      {error && <p role="alert">{error}</p>}
      <ul>
        {items.map((a) => <Item key={`${a.kind}:${a.version}`} a={a} />)}
      </ul>
    </section>
  );
}
