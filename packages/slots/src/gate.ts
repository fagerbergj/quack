import type { AcquireResult } from './table.ts';

export type SlotClient = {
  acquire(endpoint: string, holder: string): Promise<AcquireResult>;
  heartbeat(endpoint: string, holder: string): Promise<boolean>;
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
  client: SlotClient;
  /** One session process is one holder, so a restarted process takes its old place back instead of double-holding. Parallel calls add `#n` to it. */
  holder: string;
  clock?: Clock;
  /** How long a run keeps its slots with no generation in flight (the idle grace). */
  graceMs?: number;
  /** The longest a slot is held before the next generation requeues behind waiters. */
  capMs?: number;
  /** Must match the service's lease TTL; heartbeats go at a third of it. */
  leaseTtlMs?: number;
  pollMs?: number;
  /** After the service fails, generations skip it this long. */
  downMs?: number;
  log?: (line: string) => void;
  setNote?: (text: string | null) => void;
};

type Lease = { idx: number; endpoint: string; since: number; busy: boolean; stopBeat: () => void };
/** One begun model call; `end` takes it back so the call frees its own slot, whatever order calls finish in. */
export type SlotCall = { lease?: Lease };

/** A run's holds on one endpoint's slots: one lease per model call in flight, and idle ones kept between generations. */
export class SlotGate {
  readonly #o: Required<Omit<GateOptions, 'clock'>> & { clock: Clock };
  #leases: Lease[] = [];
  #calls = new Set<SlotCall>();
  #cancelTimer = () => {};
  #downUntil = 0;
  #warned = false;

  constructor(options: GateOptions) {
    this.#o = { graceMs: 60_000, capMs: 600_000, leaseTtlMs: 30_000, pollMs: 1000, downMs: 30_000, log: console.error, setNote: () => {}, ...options, clock: options.clock ?? realClock };
  }

  // The first lease keeps the session's holder name, so a restarted process takes its old place back.
  #holder = (l: Lease) => (l.idx ? `${this.#o.holder}#${l.idx}` : this.#o.holder);

  /** Holds a slot on `endpoint` before a model request, one per call in flight; resolves when granted, or at once if the service is down (fail-open). */
  async begin(endpoint: string, signal?: AbortSignal): Promise<SlotCall> {
    const call: SlotCall = {};
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
  end(call?: SlotCall): void {
    const done = call ?? [...this.#calls].at(-1);
    if (!done || !this.#calls.delete(done)) return;
    if (done.lease) done.lease.busy = false;
    if (this.#calls.size === 0 && this.#leases.length) this.#arm(this.#o.graceMs);
  }

  /** The run waits on something other than its own next model call (a person, a child run), or ended: it gives the slots back. */
  idle(): Promise<void> {
    return this.#calls.size === 0 ? this.release() : Promise.resolve();
  }

  async release(): Promise<void> {
    this.#cancelTimer();
    await Promise.all([...this.#leases].map((l) => this.#drop(l)));
  }

  async #lease(endpoint: string, signal?: AbortSignal): Promise<Lease | undefined> {
    const now = this.#o.clock.now();
    for (const l of [...this.#leases]) if (!l.busy && (l.endpoint !== endpoint || now - l.since > this.#o.capMs)) await this.#drop(l);
    const idle = this.#leases.find((l) => !l.busy && l.endpoint === endpoint);
    if (idle) return Object.assign(idle, { busy: true });
    // Pushed before the first await, so a concurrent call takes the next index.
    const idx = Array.from({ length: this.#leases.length + 1 }, (_, i) => i).find((i) => !this.#leases.some((l) => l.idx === i))!;
    const lease: Lease = { idx, endpoint, since: now, busy: true, stopBeat: () => {} };
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

  #remove(lease: Lease) {
    lease.stopBeat();
    this.#leases = this.#leases.filter((l) => l !== lease);
  }

  async #drop(lease: Lease): Promise<void> {
    this.#remove(lease);
    await this.#o.client.release(lease.endpoint, this.#holder(lease)).catch(() => {});
  }

  #arm(ms: number) {
    this.#cancelTimer();
    this.#cancelTimer = this.#o.clock.setTimer(() => { this.#calls.clear(); void this.release(); }, ms);
  }

  #beat(lease: Lease) {
    lease.stopBeat = this.#o.clock.setTimer(async () => {
      const alive = await this.#o.client.heartbeat(lease.endpoint, this.#holder(lease)).catch(() => true);
      if (!alive) return void (this.#remove(lease), this.#o.log(`slots: lost the ${lease.endpoint} lease; the next generation asks again`));
      if (this.#leases.includes(lease)) this.#beat(lease);
    }, this.#o.leaseTtlMs / 3);
  }

  // False when the service failed and the generation goes ungated.
  async #acquire(lease: Lease, signal?: AbortSignal): Promise<Lease | undefined> {
    for (;;) {
      signal?.throwIfAborted();
      let r: AcquireResult;
      try {
        r = await this.#o.client.acquire(lease.endpoint, this.#holder(lease));
      } catch (err) {
        this.#downUntil = this.#o.clock.now() + this.#o.downMs;
        this.#o.setNote(null);
        if (!this.#warned) this.#o.log(`slots: service unreachable, generations run ungated: ${(err as Error).message}`);
        this.#warned = true;
        return undefined;
      }
      if (r.status === 'granted') {
        lease.since = this.#o.clock.now();
        this.#o.setNote(null);
        this.#beat(lease);
        return lease;
      }
      this.#o.setNote(`Waiting for a model slot (${r.ahead} ahead)`);
      await this.#o.clock.sleep(this.#o.pollMs, signal);
      // A call of this session that ended frees its own idle slot; take that over the queue.
      const spare = this.#leases.find((l) => l !== lease && !l.busy && l.endpoint === lease.endpoint);
      if (spare) {
        await this.#drop(lease);
        this.#o.setNote(null);
        return Object.assign(spare, { busy: true });
      }
    }
  }
}

/** The service over HTTP; any non-2xx or network error throws, which the gate treats as the service being down. */
export function httpClient(url: string, token: string, doFetch: typeof fetch = fetch): SlotClient {
  const post = async (path: string, endpoint: string, holder: string) => {
    const res = await doFetch(`${url}/${path}`, { method: 'POST', headers: { authorization: `Bearer ${token}` }, body: JSON.stringify({ endpoint, holder }), signal: AbortSignal.timeout(5000) });
    if (!res.ok) throw new Error(`slots service answered ${res.status}`);
    return res.json();
  };
  return {
    acquire: (e, h) => post('acquire', e, h) as Promise<AcquireResult>,
    heartbeat: async (e, h) => ((await post('heartbeat', e, h)) as { held: boolean }).held,
    release: async (e, h) => void (await post('release', e, h)),
  };
}
