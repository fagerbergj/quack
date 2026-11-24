import type { ModelWait } from '@quack/sdk';
import type { Lease } from './ledger.ts';

export type LeaseClient = {
  /** With `ahead`, the place last seen, the service holds the request until that place changes or its hold time passes. */
  acquire(endpoint: string, holder: string, opts?: { ahead?: number; signal?: AbortSignal }): Promise<Lease>;
  /** The extended lease, or undefined when it is lost (lapsed or released). */
  heartbeat(endpoint: string, holder: string): Promise<Lease | undefined>;
  release(endpoint: string, holder: string): Promise<void>;
};

/** Time seam: tests drive it by hand, so no test sleeps or races a real timer. */
export type Clock = {
  now(): number;
  setTimer(fn: () => void, ms: number): () => void;
};

export const realClock: Clock = {
  now: Date.now,
  setTimer: (fn, ms) => {
    const t = setTimeout(fn, ms);
    t.unref();
    return () => clearTimeout(t);
  },
};

export type GateOptions = {
  client: LeaseClient;
  /** One session process is one holder, so a restarted process takes its old place back instead of double-holding. Parallel calls add `#n` to it. */
  holder: string;
  clock?: Clock;
  /** After the service fails, generations skip it this long. */
  downMs?: number;
  log?: (line: string) => void;
  /** Hears the run's wait for a slot: the oldest queued call's wait (since, ahead and provider all from that call), or null when none queues. */
  setWait?: (wait: ModelWait | null) => void;
};

/** `busy` or `rides` (0 or 1) is the one call in flight on the lease, which is then neither reused nor released by `end`; `riders` are the shared calls queued for their turn.
 * A `lent` lease is one a shared call was granted during a wait: it goes back once free, instead of staying as the run's kept lease. */
type Held = {
  idx: number; endpoint: string; busy: boolean; rides: number; riders: Array<() => void>; granted?: true; lent?: true; stopBeat: () => void;
  // While queued at the service: `take` hands it a lease of the run's that came free, `cut` ends its held request.
  take?: (spare: Held) => void; cut?: () => void;
};
/** One begun model call; `end` takes it back so the call frees its own lease, whatever order calls finish in. */
export type GatedCall = { lease?: Held; ride?: true };
const free = (l: Held) => !l.busy && !l.rides;
// A floor on the heartbeat cadence, whatever expireTime the service grants.
const MIN_BEAT_MS = 1000;
// Above the service's hold time, so a held acquire is answered before the client gives up on it.
const HELD_TIMEOUT_MS = 60_000;

/** A run's leases on model endpoints: one per model call in flight, and one per endpoint kept between generations until the run ends or waits. */
export class CapacityGate {
  readonly #o: Required<Omit<GateOptions, 'clock'>> & { clock: Clock };
  #waiting = false;
  #leases: Held[] = [];
  #calls = new Set<GatedCall>();
  // A shared call's pending lease per endpoint, which the round's other shared calls wait for instead of queueing their own.
  #taking = new Map<string, Promise<Held | undefined>>();
  // Releases go out in order and land before the next acquire, so a reused holder name is never released after its new grant.
  #releasing = Promise.resolve();
  #downUntil = 0;
  #warned = false;
  // Keyed by a lease queued at the service, or by a ride waiting its turn on a held lease.
  #queued = new Map<object, ModelWait>();

  constructor(options: GateOptions) {
    this.#o = { downMs: 30_000, log: console.error, setWait: () => {}, ...options, clock: options.clock ?? realClock };
  }

  // The first lease keeps the session's holder name, so a restarted process takes its old place back.
  #holder = (l: Held) => (l.idx ? `${this.#o.holder}#${l.idx}` : this.#o.holder);

  /** Holds a lease on `endpoint` before a model request, one per call in flight; resolves when granted, or at once if the service is down (fail-open).
   * A `shared` call (the judge's) runs under the run's own lease, taking one only when the run holds none, and waits while another call is in flight on it. */
  async begin(endpoint: string, signal?: AbortSignal, shared = false): Promise<GatedCall> {
    const call: GatedCall = {};
    this.#calls.add(call);
    try {
      if (this.#o.clock.now() >= this.#downUntil) call.lease = await (shared ? this.#share(endpoint, signal) : this.#lease(endpoint, signal));
    } catch (err) {
      this.#calls.delete(call);
      throw err;
    }
    if (shared && call.lease) call.ride = true;
    return call;
  }

  /** A model response ended. Without `call`, the newest open one that holds a lease ends, so a ride still waiting its turn is never taken for it. */
  end(call?: GatedCall): void {
    const done = call ?? [...this.#calls].findLast((c) => c.lease) ?? [...this.#calls].at(-1);
    if (!done || !this.#calls.delete(done) || !done.lease) return;
    const lease = done.lease;
    if (done.ride) lease.rides = 0;
    else lease.busy = false;
    this.#nextRide(lease);
    // The run keeps its lowest-numbered free lease per endpoint that it is not lending; a parallel call's extra one goes back with the call.
    const idleOn = this.#leases.filter((l) => free(l) && l.endpoint === lease.endpoint);
    const keep = idleOn.filter((l) => !l.lent).sort((a, b) => a.idx - b.idx)[0];
    for (const l of idleOn) if (l !== keep) void this.#drop(l);
    // A call of this run queued at the service takes the kept lease over its place, marked busy here so no concurrent begin shares it.
    const queued = keep && this.#leases.find((l) => l.take && l.endpoint === keep.endpoint);
    if (queued) queued.take!(Object.assign(keep, { busy: true }));
  }

  /** The run opened or closed a wait (a person, a child run). Waits count per process, so a child run's calls see its parent's;
   * a lease granted to a shared call during one is lent, but a wait that opens after the grant leaves that lease kept. */
  setWaiting(open: boolean): void {
    this.#waiting = open;
    if (open) void this.idle();
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

  // Every shared call rides a granted lease, the one it took included; with none held, the first takes one and the rest wait for it.
  async #share(endpoint: string, signal?: AbortSignal): Promise<Held | undefined> {
    const ours = this.#leases.filter((l) => l.granted && l.endpoint === endpoint);
    const held = ours.find(free) ?? ours[0];
    if (held) return this.#ride(held, signal);
    const pending = this.#taking.get(endpoint);
    if (pending) {
      const got = await pending.catch(() => undefined);
      return got && this.#leases.includes(got) ? this.#ride(got, signal) : undefined;
    }
    const taking = this.#lease(endpoint, signal, true).then((l) => l && Object.assign(l, { busy: false, rides: 1 }));
    this.#taking.set(endpoint, taking);
    try {
      return await taking;
    } finally {
      this.#taking.delete(endpoint);
    }
  }

  // A lease carries one model call at a time, so a judge round adds no lane or KV load beyond its session's slot; the rest queue in order.
  async #ride(lease: Held, signal?: AbortSignal): Promise<Held | undefined> {
    if (free(lease)) return Object.assign(lease, { rides: 1 });
    signal?.throwIfAborted();
    return new Promise((resolve, reject) => {
      const leave = () => (this.#queued.delete(go), this.#listRiders(lease));
      const stop = () => (lease.riders.splice(lease.riders.indexOf(go), 1), leave(), reject(signal!.reason));
      const go = () => (signal?.removeEventListener('abort', stop), leave(), resolve(this.#leases.includes(lease) ? lease : undefined));
      lease.riders.push(go);
      this.#listRiders(lease);
      signal?.addEventListener('abort', stop, { once: true });
    });
  }

  // A ride waiting its turn reports as a call queued at the service does: since it began, and the rides ahead of it.
  #listRiders(lease: Held) {
    lease.riders.forEach((go, ahead) => this.#queued.set(go, { since: this.#queued.get(go)?.since ?? this.#o.clock.now(), ahead, provider: lease.endpoint }));
    this.#publish();
  }

  // A call's end hands the lease straight to the next queued rider, so a non-shared call never finds it free in between.
  #nextRide(lease: Held) {
    const next = lease.riders.shift();
    if (!next) return;
    lease.rides = 1;
    next();
  }

  // A run keeps one slot per gated endpoint until it ends or waits: gating a second provider the judge or decide uses
  // would park a slot there for every judging run.
  async #lease(endpoint: string, signal?: AbortSignal, shared = false): Promise<Held | undefined> {
    const idle = this.#leases.find((l) => free(l) && l.endpoint === endpoint);
    if (idle) return Object.assign(idle, { busy: true });
    // Pushed before the first await, so a concurrent call takes the next index.
    const idx = Array.from({ length: this.#leases.length + 1 }, (_, i) => i).find((i) => !this.#leases.some((l) => l.idx === i))!;
    const lease: Held = { idx, endpoint, busy: true, rides: 0, riders: [], stopBeat: () => {} };
    this.#leases.push(lease);
    try {
      const got = await this.#acquire(lease, signal);
      // Decided at the grant, not the take: a wait that opened while this take queued still lends it; a spare it adopted stays the run's.
      if (shared && got === lease && this.#waiting) lease.lent = true;
      if (got) return got;
    } catch (err) {
      void this.#releaseAt(endpoint, this.#holder(lease));
      this.#remove(lease);
      throw err;
    }
    this.#remove(lease);
    return undefined;
  }

  // Riders still queued on a lease that goes away run ungated, as a call does when the service is down.
  #remove(lease: Held) {
    lease.stopBeat();
    lease.cut?.();
    this.#leases = this.#leases.filter((l) => l !== lease);
    for (const go of lease.riders.splice(0)) go();
  }

  async #drop(lease: Held): Promise<void> {
    this.#remove(lease);
    await this.#releaseAt(lease.endpoint, this.#holder(lease));
  }

  #releaseAt(endpoint: string, holder: string): Promise<void> {
    return (this.#releasing = this.#releasing.then(() => this.#o.client.release(endpoint, holder).catch(() => {})));
  }

  // A run end dropped this lease while it queued: whatever the service now holds for its name goes back, unless a live lease took the name.
  async #orphaned(lease: Held): Promise<boolean> {
    if (this.#leases.includes(lease)) return false;
    const holder = this.#holder(lease);
    if (!this.#leases.some((l) => l.endpoint === lease.endpoint && this.#holder(l) === holder)) await this.#releaseAt(lease.endpoint, holder);
    return true;
  }

  // The cadence follows the expireTime the service granted; an ungated lease has none and never lapses.
  #beat(lease: Held, granted: Lease) {
    if (!granted.expireTime) return;
    const ms = Math.max(MIN_BEAT_MS, (Date.parse(granted.expireTime) - this.#o.clock.now()) / 3);
    lease.stopBeat = this.#o.clock.setTimer(async () => {
      // A failed beat reads as alive: the service being down must not cost the lease.
      const next = await this.#o.client.heartbeat(lease.endpoint, this.#holder(lease)).catch(() => granted);
      if (!next) return void (this.#remove(lease), this.#o.log(`capacity: lost the ${lease.endpoint} lease; the next generation asks again`));
      if (this.#leases.includes(lease)) this.#beat(lease, next);
    }, ms);
  }

  // False when the service failed and the generation goes ungated.
  async #acquire(lease: Held, signal?: AbortSignal): Promise<Held | undefined> {
    try {
      return await this.#poll(lease, signal);
    } finally {
      this.#queued.delete(lease);
      this.#publish();
    }
  }

  #queue(lease: Held, ahead: number) {
    this.#queued.set(lease, { since: this.#queued.get(lease)?.since ?? this.#o.clock.now(), ahead, provider: lease.endpoint });
    this.#publish();
  }

  #publish() {
    const all = [...this.#queued.values()];
    const first = all.reduce((a, w) => (w.since < a.since ? w : a), all[0]!);
    this.#o.setWait(all.length ? { ...first } : null);
  }

  // Each acquire after the first is held by the service until the place it reports changes, so nothing here polls.
  async #poll(lease: Held, signal?: AbortSignal): Promise<Held | undefined> {
    const cut = new AbortController();
    // Read after every wake, so a lease handed over in the gap before this resumes is never left busy and unused.
    let got: Held | undefined;
    const spare = new Promise<'spare'>((resolve) => (lease.take = (s) => ((got = s), resolve('spare'))));
    lease.cut = () => cut.abort();
    const stop = signal ? AbortSignal.any([signal, cut.signal]) : cut.signal;
    try {
      let ahead: number | undefined;
      for (;;) {
        signal?.throwIfAborted();
        await this.#releasing;
        const asked = this.#o.client.acquire(lease.endpoint, this.#holder(lease), { ahead, signal: stop });
        asked.catch(() => {});
        const r = await Promise.race([asked, spare]).catch((err: unknown) => this.#failed(err, signal, cut.signal));
        if (got) {
          lease.take = undefined;
          // Its own place goes back only once the request settles, so the release cannot land before the acquire.
          cut.abort();
          await asked.catch(() => {});
          await this.#drop(lease);
          return got;
        }
        if (typeof r !== 'object') return undefined;
        if (!this.#leases.includes(lease)) return void (await this.#orphaned(lease));
        if (r.state === 'granted') {
          lease.granted = true;
          this.#beat(lease, r);
          return lease;
        }
        ahead = r.ahead;
        this.#queue(lease, ahead);
      }
    } finally {
      lease.take = lease.cut = undefined;
    }
  }

  // An abort of the call rethrows; this gate cutting the request short is `cut`; anything else is the service down, and undefined.
  #failed(err: unknown, signal: AbortSignal | undefined, cut: AbortSignal): 'cut' | undefined {
    if (signal?.aborted) throw signal.reason;
    if (cut.aborted) return 'cut';
    this.#downUntil = this.#o.clock.now() + this.#o.downMs;
    if (!this.#warned) this.#o.log(`capacity: service unreachable, generations run ungated: ${(err as Error).message}`);
    this.#warned = true;
    return undefined;
  }
}

/** The service over HTTP; a network error or an unexpected status throws, which the gate treats as the service being down. */
export function httpClient(url: string, token: string, doFetch: typeof fetch = fetch): LeaseClient {
  const call = async (method: string, endpoint: string, holder: string, verb = '', o: { ahead?: number; signal?: AbortSignal } = {}) => {
    const at = `${url}/endpoints/${encodeURIComponent(endpoint)}/leases/${encodeURIComponent(holder)}${verb}${o.ahead === undefined ? '' : `?ahead=${o.ahead}`}`;
    const timeout = AbortSignal.timeout(o.ahead === undefined ? 5000 : HELD_TIMEOUT_MS);
    const res = await doFetch(at, { method, headers: { authorization: `Bearer ${token}` }, signal: o.signal ? AbortSignal.any([o.signal, timeout]) : timeout });
    // 404 and 409 on a heartbeat are the lease being lost, not the service.
    if (verb && (res.status === 404 || res.status === 409)) return undefined;
    if (!res.ok) throw new Error(`capacity service answered ${res.status}`);
    return res.status === 204 ? undefined : ((await res.json()) as Lease);
  };
  return {
    acquire: async (e, h, o) => (await call('PUT', e, h, '', o))!,
    heartbeat: (e, h) => call('POST', e, h, ':heartbeat'),
    release: async (e, h) => void (await call('DELETE', e, h)),
  };
}
