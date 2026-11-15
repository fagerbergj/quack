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
  /** How long a run keeps its leases with no generation in flight (the idle grace). */
  graceMs?: number;
  /** The longest a lease is held before the next generation requeues behind waiters. */
  capMs?: number;
  pollMs?: number;
  /** After the service fails, generations skip it this long. */
  downMs?: number;
  log?: (line: string) => void;
  setNote?: (text: string | null) => void;
};

type Held = { idx: number; endpoint: string; since: number; busy: boolean; stopBeat: () => void };
/** One begun model call; `end` takes it back so the call frees its own lease, whatever order calls finish in. */
export type GatedCall = { lease?: Held };

/** A run's leases on model endpoints: one per model call in flight, and idle ones kept between generations. */
export class CapacityGate {
  readonly #o: Required<Omit<GateOptions, 'clock'>> & { clock: Clock };
  #leases: Held[] = [];
  #calls = new Set<GatedCall>();
  #cancelTimer = () => {};
  #downUntil = 0;
  #warned = false;

  constructor(options: GateOptions) {
    this.#o = { graceMs: 60_000, capMs: 600_000, pollMs: 1000, downMs: 30_000, log: console.error, setNote: () => {}, ...options, clock: options.clock ?? realClock };
  }

  // The first lease keeps the session's holder name, so a restarted process takes its old place back.
  #holder = (l: Held) => (l.idx ? `${this.#o.holder}#${l.idx}` : this.#o.holder);

  /** Holds a lease on `endpoint` before a model request, one per call in flight; resolves when granted, or at once if the service is down (fail-open). */
  async begin(endpoint: string, signal?: AbortSignal): Promise<GatedCall> {
    const call: GatedCall = {};
    this.#calls.add(call);
    this.#cancelTimer();
    try {
      if (this.#o.clock.now() >= this.#downUntil) call.lease = await this.#lease(endpoint, signal);
    } catch (err) {
      this.#calls.delete(call);
      this.#o.setNote(null);
      throw err;
    }
    this.#arm(this.#o.capMs);
    return call;
  }

  /** A model response ended; with none in flight the idle grace starts. Without `call`, the newest open one ends. */
  end(call?: GatedCall): void {
    const done = call ?? [...this.#calls].at(-1);
    if (!done || !this.#calls.delete(done)) return;
    if (done.lease) done.lease.busy = false;
    if (this.#calls.size === 0 && this.#leases.length) this.#arm(this.#o.graceMs);
  }

  /** The run waits on something other than its own next model call (a person, a child run), or ended: it gives its leases back. */
  idle(): Promise<void> {
    return this.#calls.size === 0 ? this.release() : Promise.resolve();
  }

  async release(): Promise<void> {
    this.#cancelTimer();
    await Promise.all([...this.#leases].map((l) => this.#drop(l)));
  }

  async #lease(endpoint: string, signal?: AbortSignal): Promise<Held | undefined> {
    const now = this.#o.clock.now();
    for (const l of [...this.#leases]) if (!l.busy && (l.endpoint !== endpoint || now - l.since > this.#o.capMs)) await this.#drop(l);
    const idle = this.#leases.find((l) => !l.busy && l.endpoint === endpoint);
    if (idle) return Object.assign(idle, { busy: true });
    // Pushed before the first await, so a concurrent call takes the next index.
    const idx = Array.from({ length: this.#leases.length + 1 }, (_, i) => i).find((i) => !this.#leases.some((l) => l.idx === i))!;
    const lease: Held = { idx, endpoint, since: now, busy: true, stopBeat: () => {} };
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

  #arm(ms: number) {
    this.#cancelTimer();
    this.#cancelTimer = this.#o.clock.setTimer(() => { this.#calls.clear(); void this.release(); }, ms);
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
        lease.since = this.#o.clock.now();
        this.#o.setNote(null);
        this.#beat(lease, r);
        return lease;
      }
      this.#o.setNote(`Waiting for model capacity (${r.ahead} ahead)`);
      await this.#o.clock.sleep(this.#o.pollMs, signal);
      // A call of this session that ended frees its own idle lease; take that over the queue.
      const spare = this.#leases.find((l) => l !== lease && !l.busy && l.endpoint === lease.endpoint);
      if (spare) {
        await this.#drop(lease);
        this.#o.setNote(null);
        return Object.assign(spare, { busy: true });
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
