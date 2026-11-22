import type { Lease } from './ledger.ts';

export type LeaseClient = {
  acquire(endpoint: string, holder: string): Promise<Lease>;
  /** The extended lease, or undefined when it is lost (lapsed or released). */
  heartbeat(endpoint: string, holder: string): Promise<Lease | undefined>;
  release(endpoint: string, holder: string): Promise<void>;
};

/** Time seam: tests drive it by hand, so no test sleeps or races a real timer. */
export type Clock = {
  now(): number;
  setTimer(fn: () => void, ms: number): () => void;
  sleep(ms: number, signal?: AbortSignal): Promise<void>;
};

export const realClock: Clock = {
  now: Date.now,
  setTimer: (fn, ms) => {
    const t = setTimeout(fn, ms);
    t.unref();
    return () => clearTimeout(t);
  },
  sleep: (ms, signal) => new Promise((resolve, reject) => {
    if (signal?.aborted) return reject(signal.reason);
    const t = setTimeout(resolve, ms);
    signal?.addEventListener('abort', () => (clearTimeout(t), reject(signal.reason)), { once: true });
  }),
};

export type GateOptions = {
  client: LeaseClient;
  /** One session process is one holder, so a restarted process takes its old place back instead of double-holding. Parallel calls add `#n` to it. */
  holder: string;
  clock?: Clock;
  pollMs?: number;
  /** After the service fails, generations skip it this long. */
  downMs?: number;
  log?: (line: string) => void;
  setNote?: (text: string | null) => void;
};

type Held = { idx: number; endpoint: string; busy: boolean; stopBeat: () => void };
/** One begun model call; `end` takes it back so the call frees its own lease, whatever order calls finish in. */
export type GatedCall = { lease?: Held };

/** A run's leases on model endpoints: one per model call in flight, and one per endpoint kept between generations until the run ends or waits. */
export class CapacityGate {
  readonly #o: Required<Omit<GateOptions, 'clock'>> & { clock: Clock };
  #leases: Held[] = [];
  #calls = new Set<GatedCall>();
  #downUntil = 0;
  #warned = false;

  constructor(options: GateOptions) {
    this.#o = { pollMs: 1000, downMs: 30_000, log: console.error, setNote: () => {}, ...options, clock: options.clock ?? realClock };
  }

  // The first lease keeps the session's holder name, so a restarted process takes its old place back.
  #holder = (l: Held) => (l.idx ? `${this.#o.holder}#${l.idx}` : this.#o.holder);

  /** Holds a lease on `endpoint` before a model request, one per call in flight; resolves when granted, or at once if the service is down (fail-open). */
  async begin(endpoint: string, signal?: AbortSignal): Promise<GatedCall> {
    const call: GatedCall = {};
    this.#calls.add(call);
    try {
      if (this.#o.clock.now() >= this.#downUntil) call.lease = await this.#lease(endpoint, signal);
    } catch (err) {
      this.#calls.delete(call);
      this.#o.setNote(null);
      throw err;
    }
    // A run that ended while this call queued cleared the calls; this one still ends through end().
    this.#calls.add(call);
    return call;
  }

  /** A model response ended. Without `call`, the newest open one ends. */
  end(call?: GatedCall): void {
    const done = call ?? [...this.#calls].at(-1);
    if (!done || !this.#calls.delete(done) || !done.lease) return;
    const { endpoint } = Object.assign(done.lease, { busy: false });
    // The run keeps its lowest-numbered lease per endpoint; a parallel call's extra one goes back with the call.
    const spare = this.#leases.filter((l) => !l.busy && l.endpoint === endpoint).sort((a, b) => a.idx - b.idx).slice(1);
    for (const l of spare) void this.#drop(l);
  }

  /** The run waits on something other than its own next model call (a person, a child run): it gives its leases back. */
  idle(): Promise<void> {
    return this.#calls.size === 0 ? this.release() : Promise.resolve();
  }

  /** The run ended: every lease goes back, also those of calls an abort cut off before their response. */
  async release(): Promise<void> {
    this.#calls.clear();
    await Promise.all([...this.#leases].map((l) => this.#drop(l)));
  }

  async #lease(endpoint: string, signal?: AbortSignal): Promise<Held | undefined> {
    const idle = this.#leases.find((l) => !l.busy && l.endpoint === endpoint);
    if (idle) return Object.assign(idle, { busy: true });
    // Pushed before the first await, so a concurrent call takes the next index.
    const idx = Array.from({ length: this.#leases.length + 1 }, (_, i) => i).find((i) => !this.#leases.some((l) => l.idx === i))!;
    const lease: Held = { idx, endpoint, busy: true, stopBeat: () => {} };
    this.#leases.push(lease);
    try {
      const got = await this.#acquire(lease, signal);
      if (got) return got;
    } catch (err) {
      void this.#o.client.release(endpoint, this.#holder(lease)).catch(() => {});
      this.#remove(lease);
      throw err;
    }
    this.#remove(lease);
    return undefined;
  }

  #remove(lease: Held) {
    lease.stopBeat();
    this.#leases = this.#leases.filter((l) => l !== lease);
  }

  async #drop(lease: Held): Promise<void> {
    this.#remove(lease);
    await this.#o.client.release(lease.endpoint, this.#holder(lease)).catch(() => {});
  }

  // The cadence follows the expireTime the service granted; an ungated lease has none and never lapses.
  #beat(lease: Held, granted: Lease) {
    if (!granted.expireTime) return;
    const ms = Math.max(this.#o.pollMs, (Date.parse(granted.expireTime) - this.#o.clock.now()) / 3);
    lease.stopBeat = this.#o.clock.setTimer(async () => {
      // A failed beat reads as alive: the service being down must not cost the lease.
      const next = await this.#o.client.heartbeat(lease.endpoint, this.#holder(lease)).catch(() => granted);
      if (!next) return void (this.#remove(lease), this.#o.log(`capacity: lost the ${lease.endpoint} lease; the next generation asks again`));
      if (this.#leases.includes(lease)) this.#beat(lease, next);
    }, ms);
  }

  // False when the service failed and the generation goes ungated.
  async #acquire(lease: Held, signal?: AbortSignal): Promise<Held | undefined> {
    for (;;) {
      signal?.throwIfAborted();
      let r: Lease;
      try {
        r = await this.#o.client.acquire(lease.endpoint, this.#holder(lease));
      } catch (err) {
        this.#downUntil = this.#o.clock.now() + this.#o.downMs;
        this.#o.setNote(null);
        if (!this.#warned) this.#o.log(`capacity: service unreachable, generations run ungated: ${(err as Error).message}`);
        this.#warned = true;
        return undefined;
      }
      if (r.state === 'granted') {
        this.#o.setNote(null);
        this.#beat(lease, r);
        return lease;
      }
      this.#o.setNote(`Waiting for model capacity (${r.ahead} ahead)`);
      await this.#o.clock.sleep(this.#o.pollMs, signal);
      // A call of this session that ended frees its own idle lease; take that over the queue.
      const spare = this.#leases.find((l) => l !== lease && !l.busy && l.endpoint === lease.endpoint);
      if (spare) {
        spare.busy = true;
        await this.#drop(lease);
        this.#o.setNote(null);
        return spare;
      }
    }
  }
}

/** The service over HTTP; a network error or an unexpected status throws, which the gate treats as the service being down. */
export function httpClient(url: string, token: string, doFetch: typeof fetch = fetch): LeaseClient {
  const call = async (method: string, endpoint: string, holder: string, verb = '') => {
    const at = `${url}/endpoints/${encodeURIComponent(endpoint)}/leases/${encodeURIComponent(holder)}${verb}`;
    const res = await doFetch(at, { method, headers: { authorization: `Bearer ${token}` }, signal: AbortSignal.timeout(5000) });
    // 404 and 409 on a heartbeat are the lease being lost, not the service.
    if (verb && (res.status === 404 || res.status === 409)) return undefined;
    if (!res.ok) throw new Error(`capacity service answered ${res.status}`);
    return res.status === 204 ? undefined : ((await res.json()) as Lease);
  };
  return {
    acquire: async (e, h) => (await call('PUT', e, h))!,
    heartbeat: (e, h) => call('POST', e, h, ':heartbeat'),
    release: async (e, h) => void (await call('DELETE', e, h)),
  };
}
