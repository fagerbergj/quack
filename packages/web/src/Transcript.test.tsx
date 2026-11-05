import { fireEvent, render, screen } from '@testing-library/react';
import type { AssistantMessage } from '@earendil-works/pi-ai';
import { expect, test, vi } from 'vitest';
import { SessionKeyContext } from './extension.ts';
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

test('a subagent call nests its child conversation, fetched from the session when opened', async () => {
  const call = { role: 'assistant', timestamp: 1, content: [{ type: 'toolCall', id: 's1', name: 'subagent', arguments: { task: 'look it up' } }] };
  const result = { role: 'toolResult', timestamp: 2, toolCallId: 's1', toolName: 'subagent', content: [{ type: 'text', text: 'found it' }], isError: false, details: { conversationId: 'child-1' } };
  const items = [{ kind: 'entry', entry: { id: 'a', kind: 'pi.assistant', model: [call] } }, { kind: 'entry', entry: { id: 'b', kind: 'pi.tool-result', model: [result] } }] as unknown as View['items'];
  const urls: string[] = [];
  vi.stubGlobal('fetch', async (url: string) => {
    urls.push(url);
    const child = { role: 'assistant', timestamp: 3, content: [{ type: 'text', text: 'child notes' }] };
    return Response.json({ transcript: [{ id: 'c', kind: 'pi.assistant', model: [child] }] });
  });
  render(<SessionKeyContext value="research:one"><Transcript view={{ ...emptyView, items }} load={() => undefined} /></SessionKeyContext>);
  const sub = screen.getByText('Subagent transcript').closest('details')!;
  sub.open = true;
  fireEvent(sub, new Event('toggle'));
  expect(await screen.findByText('child notes')).toBeInTheDocument();
  expect(urls).toEqual(['sessions/research%3Aone/transcript?conversation=child-1']);
  vi.unstubAllGlobals();
});
