import type { Meta, StoryObj } from '@storybook/react-vite'
import { Dialog, DialogClose, DialogContent } from './dialog'
import { Button } from './button'

const meta: Meta<typeof DialogContent> = {
  title: 'UI/Dialog',
  component: DialogContent,
  parameters: { layout: 'fullscreen', renderCheck: { viewports: ['mobile', 'desktop'] } },
}
export default meta

type Story = StoryObj<typeof DialogContent>

// A bottom sheet below `medium`, a centred dialog above it.
export const Responsive: Story = {
  render: () => (
    <Dialog open>
      <DialogContent title="Example dialog" className="gap-3 p-5">
        <p className="text-sm">Dialog body</p>
        <DialogClose asChild><Button variant="outline" className="self-start">Close</Button></DialogClose>
      </DialogContent>
    </Dialog>
  ),
}

// The version-skew wall: covers everything and ignores Esc and outside clicks.
export const Blocking: Story = {
  render: () => (
    <Dialog open>
      <DialogContent title="Blocked" placement="blocking" onEscapeKeyDown={e => e.preventDefault()} onInteractOutside={e => e.preventDefault()}>
        <p>This page no longer works with the server.</p>
      </DialogContent>
    </Dialog>
  ),
}
