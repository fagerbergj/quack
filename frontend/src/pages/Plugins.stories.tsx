import type { Meta, StoryObj } from '@storybook/react-vite'
import Plugins from './Plugins'
import type { Plugin, PluginUpdate } from '../api'

const meta: Meta<typeof Plugins> = {
  title: 'Pages/Plugins',
  component: Plugins,
  parameters: { layout: 'fullscreen' },
  decorators: [Story => <div className="h-[37.5rem]"><Story /></div>],
}
export default meta

type Story = StoryObj<typeof Plugins>

const samplePlugins: Plugin[] = [
  { name: 'quack', entry: '', source: 'embedded' },
  {
    name: 'dotagents', entry: 'github:fagerbergj/dotagents', source: 'github',
    owner: 'fagerbergj', repo: 'dotagents',
    installed_sha: 'c886ce1a8474939dc42f7c194f8c57242223ea1',
    fetched_at: new Date(Date.now() - 3 * 3600_000).toISOString(),
    root: '/data/.quack/plugins/dotagents/repo',
  },
  {
    name: 'ponytail', entry: 'github:fagerbergj/ponytail@v1.4', source: 'github',
    owner: 'fagerbergj', repo: 'ponytail', ref: 'v1.4',
    installed_sha: '0a4dd63ad4541f4f655c4108a295916f3c1d8fd',
    fetched_at: new Date(Date.now() - 26 * 3600_000).toISOString(),
    root: '/data/.quack/plugins/ponytail/repo',
  },
  {
    name: 'broken', entry: 'github:acme/broken', source: 'github',
    owner: 'acme', repo: 'broken',
    error: 'git clone: repository not found',
    root: '/data/.quack/plugins/broken/repo',
  },
]

const sampleUpdates: PluginUpdate[] = [
  { name: 'dotagents', installed_sha: samplePlugins[1].installed_sha, behind: false },
  { name: 'ponytail', installed_sha: samplePlugins[2].installed_sha, remote_sha: 'deadbeef1234567890deadbeef1234567890dead', behind: true },
]

export const Default: Story = {
  args: { navOpen: false, onToggleNav: () => {}, initialPlugins: samplePlugins, initialUpdates: sampleUpdates },
}

export const Dark: Story = {
  ...Default,
  globals: { theme: 'dark' },
}

export const Empty: Story = {
  args: { navOpen: false, onToggleNav: () => {}, initialPlugins: [], initialUpdates: [] },
}

export const MobileViewport390: Story = {
  args: { navOpen: false, onToggleNav: () => {}, initialPlugins: samplePlugins, initialUpdates: sampleUpdates },
  decorators: [Story => (
    <div className="w-[390px] h-[844px] overflow-hidden border border-gray-300 dark:border-gray-600">
      <Story />
    </div>
  )],
}
