import { fireEvent, render, screen } from '@testing-library/react';
import type { AssistantMessage } from '@earendil-works/pi-ai';
import { expect, test } from 'vitest';
import { Transcript } from './Transcript.tsx';
import { emptyView, type View } from './view.ts';

const asst = { role: 'assistant', timestamp: 100, content: [{ type: 'toolCall', id: 'c1', name: 'bash', arguments: { command: 'ls' } }] } as unknown as AssistantMessage;
const committed = { kind: 'entry', entry: { id: 'e1', kind: 'pi.message', model: [asst] } } as unknown as View['items'][number];

test('a fold the person opened stays open when the streamed message commits', () => {
  const { rerender } = render(<Transcript view={{ ...emptyView, partial: asst }} load={() => undefined} />);
  const fold = () => screen.getByText(/Worked/).closest('details')!;
  fireEvent.click(fold().querySelector('summary')!);
  fold().open = true; // jsdom does not toggle on a summary click
  rerender(<Transcript view={{ ...emptyView, items: [committed] }} load={() => undefined} />);
  expect(fold().open).toBe(true);
});
