import type { Meta, StoryObj } from '@storybook/react-vite'
import Memory from './Memory'

// Stubs global.fetch rather than pulling in MSW for one story; routed by URL since MemoryTab also fetches
// /memories/stats.
function stubFetch() {
  window.fetch = async (input: RequestInfo | URL) => {
    const url = typeof input === 'string' ? input : input instanceof URL ? input.toString() : input.url
    const body = url.includes('/memories/stats') ? { weeks: [], scopes: [] } : { memories: [], total: 0 }
    return new Response(JSON.stringify(body), { headers: { 'Content-Type': 'application/json' } })
  }
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
