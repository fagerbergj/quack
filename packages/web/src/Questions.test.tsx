import { render } from '@testing-library/react';
import { afterEach, expect, test, vi } from 'vitest';
import type { Question } from './api.ts';
import { SessionQuestions } from './Questions.tsx';

afterEach(() => vi.useRealTimers());

const Q: Question = { id: 'q1', question: 'Pick one', options: ['a', 'b'], evidence: null, askedAt: 1_000 };

test('SessionQuestions runs its 15 s age timer only while open questions exist', () => {
  vi.useFakeTimers();
  const { rerender, unmount } = render(<SessionQuestions sessionKey="ui:u1" items={[]} />);
  expect(vi.getTimerCount()).toBe(0);
  rerender(<SessionQuestions sessionKey="ui:u1" items={[Q]} />);
  expect(vi.getTimerCount()).toBe(1);
  rerender(<SessionQuestions sessionKey="ui:u1" items={[]} />);
  expect(vi.getTimerCount()).toBe(0);
  unmount();
});
