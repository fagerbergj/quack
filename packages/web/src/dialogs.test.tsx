import { render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { useState } from 'react';
import { expect, test } from 'vitest';
import { App } from './App.tsx';
import { Dialog } from './Dialog.tsx';
import { entry, FakeSocket, routes, sess, snapshot, useHarness } from './test-harness.ts';

useHarness(() => ({ 'GET /agents': { agents: [] } }));

function Harness() {
  const [open, setOpen] = useState(false);
  return (
    <>
      <button type="button" onClick={() => setOpen(true)}>Open</button>
      {open && (
        <Dialog label="Sheet" onClose={() => setOpen(false)} className="">
          <input aria-label="Name" autoFocus />
          <button type="button">Save</button>
        </Dialog>
      )}
    </>
  );
}

test('the dialog keeps Tab inside, closes on Escape or the backdrop, and gives focus back to its opener', async () => {
  render(<Harness />);
  const opener = screen.getByRole('button', { name: 'Open' });
  await userEvent.click(opener);
  expect(screen.getByRole('textbox', { name: 'Name' })).toHaveFocus();
  await userEvent.tab();
  expect(screen.getByRole('button', { name: 'Save' })).toHaveFocus();
  await userEvent.tab();
  expect(screen.getByRole('textbox', { name: 'Name' })).toHaveFocus();
  await userEvent.tab({ shift: true });
  expect(screen.getByRole('button', { name: 'Save' })).toHaveFocus();
  await userEvent.keyboard('{Escape}');
  expect(screen.queryByRole('dialog')).toBeNull();
  expect(opener).toHaveFocus();
  await userEvent.click(opener);
  await userEvent.click(document.querySelector('[aria-hidden="true"]')!);
  expect(screen.queryByRole('dialog')).toBeNull();
});

async function openManaged() {
  routes['GET /sessions'] = { sessions: [sess('ui:u1', { title: 'Plan the trip' })] };
  location.hash = '#/s/ui%3Au1';
  render(<App />);
  FakeSocket.all[0]!.open();
  FakeSocket.all[0]!.frame(snapshot());
  return screen.findByRole('button', { name: 'More actions' });
}

test('the session menu moves with the arrow keys, closes on Escape back to its button, and on a press outside', async () => {
  const more = await openManaged();
  await userEvent.click(more);
  const items = screen.getAllByRole('menuitem');
  expect(items[0]).toHaveFocus();
  await userEvent.keyboard('{ArrowDown}');
  expect(items[1]).toHaveFocus();
  await userEvent.keyboard('{End}');
  expect(items.at(-1)).toHaveFocus();
  await userEvent.keyboard('{ArrowDown}');
  expect(items[0]).toHaveFocus();
  await userEvent.keyboard('{Escape}');
  expect(screen.queryByRole('menu')).toBeNull();
  expect(more).toHaveFocus();
  await userEvent.click(more);
  await userEvent.click(screen.getByRole('heading', { name: /Plan the trip/ }));
  expect(screen.queryByRole('menu')).toBeNull();
});

test('Rename opens a dialog on the title field and Escape returns focus to the menu button', async () => {
  const more = await openManaged();
  await userEvent.click(more);
  await userEvent.click(screen.getByRole('menuitem', { name: 'Rename' }));
  expect(screen.getByRole('textbox', { name: 'Title' })).toHaveFocus();
  await userEvent.tab({ shift: true });
  expect(screen.getByRole('button', { name: 'Save' })).toHaveFocus();
  await userEvent.keyboard('{Escape}');
  expect(screen.queryByRole('dialog')).toBeNull();
  expect(more).toHaveFocus();
});

test('a full-size image traps focus on its close button and returns it to the thumbnail', async () => {
  location.hash = '#/s/ui%3Au1';
  routes['GET /sessions'] = { sessions: [] };
  render(<App />);
  const ws = FakeSocket.all[0]!;
  ws.open();
  ws.frame(snapshot({ entries: [entry('1', [{ role: 'user', content: [{ type: 'image', data: 'QUJD', mimeType: 'image/png' }], timestamp: 1 }])] }));
  const thumb = screen.getByRole('button', { name: 'Open image 1' });
  await userEvent.click(thumb);
  const close = screen.getByRole('button', { name: 'Close image' });
  expect(close).toHaveFocus();
  await userEvent.tab();
  expect(close).toHaveFocus();
  await userEvent.keyboard('{Escape}');
  expect(screen.queryByRole('dialog')).toBeNull();
  expect(thumb).toHaveFocus();
});
