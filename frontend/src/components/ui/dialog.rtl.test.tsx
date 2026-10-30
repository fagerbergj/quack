// @vitest-environment jsdom
import { useState } from 'react'
import { describe, it, expect, afterEach } from 'vitest'
import { cleanup, render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { Dialog, DialogContent } from './dialog'
import { Sheet, SheetContent } from './sheet'
import { DropdownMenu, DropdownMenuContent, DropdownMenuItem, DropdownMenuTrigger } from './dropdown-menu'

afterEach(cleanup)

// Opened from app state, not a Dialog.Trigger: Radix alone would drop focus on close.
function Harness({ sheet }: { sheet?: boolean }) {
  const [open, setOpen] = useState(false)
  const Root = sheet ? Sheet : Dialog
  const Content = sheet ? SheetContent : DialogContent
  return (
    <>
      <button onClick={() => setOpen(true)}>Open</button>
      <Root open={open} onOpenChange={setOpen}>
        <Content title="Example"><button>Inside</button></Content>
      </Root>
    </>
  )
}

describe.each([['Dialog', false], ['Sheet', true]])('%s', (_name, sheet) => {
  it('is named by its title, closes on Escape, and returns focus to the opener', async () => {
    const user = userEvent.setup()
    render(<Harness sheet={sheet} />)
    const opener = screen.getByRole('button', { name: 'Open' })
    await user.click(opener)
    expect(screen.getByRole('dialog', { name: 'Example' })).toBeTruthy()

    await user.keyboard('{Escape}')
    expect(screen.queryByRole('dialog')).toBeNull()
    await waitFor(() => expect(document.activeElement).toBe(opener))
  })
})

// Safari and macOS Firefox don't focus a clicked button, so the dialog opens with <body> focused.
it('does not move focus to an earlier menu trigger when the opener was never focused', async () => {
  const user = userEvent.setup()
  render(
    <>
      <DropdownMenu>
        <DropdownMenuTrigger>Menu</DropdownMenuTrigger>
        <DropdownMenuContent><DropdownMenuItem>Item</DropdownMenuItem></DropdownMenuContent>
      </DropdownMenu>
      <Harness />
    </>,
  )
  await user.click(screen.getByRole('button', { name: 'Menu' }))
  await user.keyboard('{Escape}')
  ;(document.activeElement as HTMLElement).blur()
  const opener = screen.getByRole('button', { name: 'Open' })
  opener.addEventListener('mousedown', e => e.preventDefault(), { once: true })
  await user.click(opener)
  expect(screen.getByRole('dialog')).toBeTruthy()
  await user.keyboard('{Escape}')
  expect(screen.queryByRole('dialog')).toBeNull()
  expect(document.activeElement).not.toBe(screen.getByRole('button', { name: 'Menu' }))
})
