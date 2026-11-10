import '@testing-library/jest-dom/vitest';
import { cleanup } from '@testing-library/react';
import { afterEach } from 'vitest';

// Vitest globals are off, so RTL does not register its own cleanup. Storage holds per-device choices that would leak into the next test.
afterEach(() => {
  cleanup();
  localStorage.clear();
});

// jsdom has no ResizeObserver; React Flow observes its pane and nodes with one.
globalThis.ResizeObserver ??= class { observe() {} unobserve() {} disconnect() {} };
