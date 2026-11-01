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
  /** One session process is one holder, so a restarted process takes its old place back instead of double-holding. */
  holder: string;
  clock?: Clock;
  /** How long a run keeps its slot with no generation in flight (the idle grace). */
  graceMs?: number;
  /** The longest a run holds a slot before its next generation requeues behind waiters. */
  capMs?: number;
  /** Must match the service's lease TTL; heartbeats go at a third of it. */
  leaseTtlMs?: number;
  pollMs?: number;
  /** After the service fails, generations skip it this long. */
  downMs?: number;
  log?: (line: string) => void;
  setNote?: (text: string | null) => void;
};

/** A run's hold on one endpoint's slot, kept between its generations. */
export class SlotGate {
  readonly #o: Required<Omit<GateOptions, 'clock'>> & { clock: Clock };
  #held?: { endpoint: string; since: number };
  #inflight = 0;
  #cancelTimer = () => {};
  #cancelBeat = () => {};
  #downUntil = 0;
  #warned = false;

  constructor(options: GateOptions) {
    this.#o = { graceMs: 60_000, capMs: 600_000, leaseTtlMs: 30_000, pollMs: 1000, downMs: 30_000, log: console.error, setNote: () => {}, ...options, clock: options.clock ?? realClock };
  }

  /** Holds a slot on `endpoint` before a model request; resolves when granted, or at once if the service is down (fail-open). */
  async begin(endpoint: string, signal?: AbortSignal): Promise<void> {
    this.#inflight += 1;
    this.#cancelTimer();
    try {
      if (this.#o.clock.now() < this.#downUntil) return;
      const stale = this.#held && (this.#held.endpoint !== endpoint || this.#o.clock.now() - this.#held.since > this.#o.capMs);
      if (stale) await this.release();
      if (!this.#held && !(await this.#acquire(endpoint, signal))) return;
      this.#arm(this.#o.capMs);
    } catch (err) {
      this.#inflight -= 1;
      this.#o.setNote(null);
      void this.#o.client.release(endpoint, this.#o.holder).catch(() => {});
      throw err;
    }
  }

  /** A model response ended; with none in flight the idle grace starts. */
  end(): void {
    this.#inflight = Math.max(0, this.#inflight - 1);
    if (this.#inflight === 0 && this.#held) this.#arm(this.#o.graceMs);
  }

  /** The run waits on something other than its own next model call (a person, a child run), or ended: it gives the slot back. */
  idle(): Promise<void> {
    return this.#inflight === 0 ? this.release() : Promise.resolve();
  }

  async release(): Promise<void> {
    const held = this.#held;
    this.#held = undefined;
    this.#cancelTimer();
    this.#cancelBeat();
    if (held) await this.#o.client.release(held.endpoint, this.#o.holder).catch(() => {});
  }

  #arm(ms: number) {
    this.#cancelTimer();
    this.#cancelTimer = this.#o.clock.setTimer(() => { this.#inflight = 0; void this.release(); }, ms);
  }

  #beat(endpoint: string) {
    this.#cancelBeat = this.#o.clock.setTimer(async () => {
      const alive = await this.#o.client.heartbeat(endpoint, this.#o.holder).catch(() => true);
      if (!alive) return void (this.#held = undefined, this.#o.log(`slots: lost the ${endpoint} lease; the next generation asks again`));
      if (this.#held) this.#beat(endpoint);
    }, this.#o.leaseTtlMs / 3);
  }

  // False when the service failed and the generation goes ungated.
  async #acquire(endpoint: string, signal?: AbortSignal): Promise<boolean> {
    for (;;) {
      signal?.throwIfAborted();
      let r: AcquireResult;
      try {
        r = await this.#o.client.acquire(endpoint, this.#o.holder);
      } catch (err) {
        this.#downUntil = this.#o.clock.now() + this.#o.downMs;
        this.#o.setNote(null);
        if (!this.#warned) this.#o.log(`slots: service unreachable, generations run ungated: ${(err as Error).message}`);
        this.#warned = true;
        return false;
      }
      if (r.status === 'granted') {
        this.#held = { endpoint, since: this.#o.clock.now() };
        this.#o.setNote(null);
        this.#beat(endpoint);
        return true;
      }
      this.#o.setNote(`Waiting for a model slot (${r.ahead} ahead)`);
      await this.#o.clock.sleep(this.#o.pollMs, signal);
    }
  }
}

/** The service over HTTP; any non-2xx or network error throws, which the gate treats as the service being down. */
export function httpClient(url: string, token: string, doFetch: typeof fetch = fetch): SlotClient {
  const post = async (path: string, endpoint: string, holder: string) => {
    const res = await doFetch(`${url}/slots/${path}`, { method: 'POST', headers: { authorization: `Bearer ${token}` }, body: JSON.stringify({ endpoint, holder }), signal: AbortSignal.timeout(5000) });
    if (!res.ok) throw new Error(`slots service answered ${res.status}`);
    return res.json();
  };
  return {
    acquire: (e, h) => post('acquire', e, h) as Promise<AcquireResult>,
    heartbeat: async (e, h) => ((await post('heartbeat', e, h)) as { held: boolean }).held,
    release: async (e, h) => void (await post('release', e, h)),
  };
}
