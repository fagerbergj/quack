import type { Clock } from '../src/index.ts';

// A hand-driven clock: time moves only in advance(), which fires due timers in order and lets promises settle between them.
export class FakeClock implements Clock {
  t = 1_000_000;
  #timers: Array<{ at: number; fn: () => void }> = [];
  now = () => this.t;
  setTimer = (fn: () => void, ms: number) => {
    const timer = { at: this.t + ms, fn };
    this.#timers.push(timer);
    return () => void (this.#timers = this.#timers.filter((x) => x !== timer));
  };
  sleep = (ms: number, signal?: AbortSignal) => new Promise<void>((resolve, reject) => {
    if (signal?.aborted) return reject(signal.reason);
    const cancel = this.setTimer(resolve, ms);
    signal?.addEventListener('abort', () => (cancel(), reject(signal.reason)), { once: true });
  });
  reset() {
    this.#timers = [];
  }
  async settle() {
    for (let i = 0; i < 5; i++) await new Promise((r) => setImmediate(r));
  }
  async advance(ms: number) {
    const end = this.t + ms;
    for (;;) {
      const next = this.#timers.filter((x) => x.at <= end).sort((a, b) => a.at - b.at)[0];
      if (!next) break;
      this.#timers = this.#timers.filter((x) => x !== next);
      this.t = Math.max(this.t, next.at);
      next.fn();
      await this.settle();
    }
    this.t = end;
    await this.settle();
  }
}
