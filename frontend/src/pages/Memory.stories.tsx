import type { Meta, StoryObj } from '@storybook/react-vite'
import Memory from './Memory'

// Memory renders MemoryTab, which talks to the real REST client - stub
// global.fetch with one canned empty page (same pattern as ArtifactPanel's
// story) rather than pulling in MSW for one page-level story.
function stubFetch() {
  window.fetch = async () =>
    new Response(JSON.stringify({ memories: [], total: 0 }), { headers: { 'Content-Type': 'application/json' } })
}

const meta: Meta<typeof Memory> = {
  title: 'Pages/Memory',
  component: Memory,
  parameters: { layout: 'fullscreen' },
  decorators: [Story => { stubFetch(); return <div className="h-[37.5rem]"><Story /></div> }],
}
export default meta

type Story = StoryObj<typeof Memory>

export const Default: Story = {
  args: { navOpen: false, onToggleNav: () => {} },
}

export const Dark: Story = {
  ...Default,
  globals: { theme: 'dark' },
}

export const MobileViewport390: Story = {
  args: { navOpen: false, onToggleNav: () => {} },
  decorators: [Story => (
    <div className="w-[390px] h-[844px] overflow-hidden border border-gray-300 dark:border-gray-600">
      <Story />
    </div>
  )],
}
