import type { Meta, StoryObj } from '@storybook/react-vite'
import ExtensionHost from './ExtensionHost'

const meta: Meta<typeof ExtensionHost> = {
  title: 'Pages/ExtensionHost',
  component: ExtensionHost,
  parameters: { layout: 'fullscreen' },
}
export default meta

type Story = StoryObj<typeof ExtensionHost>

// #870: an extension's own UI in a same-origin iframe, inside the SPA shell -
// NavRail stays put and back-nav works, unlike the old <a href> that left
// the app entirely. about:blank stands in for a real extension route here
// since Storybook has no /usage server route to actually load.
export const Default: Story = {
  args: {
    name: 'usage',
    initialExtensions: [{ name: 'usage', title: 'Usage', href: 'about:blank' }],
  },
  render: args => (
    <div className="h-96 flex flex-col">
      <ExtensionHost {...args} />
    </div>
  ),
}

export const NotFound: Story = {
  args: {
    name: 'missing',
    initialExtensions: [{ name: 'usage', title: 'Usage', href: 'about:blank' }],
  },
  render: args => (
    <div className="h-96 flex flex-col">
      <ExtensionHost {...args} />
    </div>
  ),
}
