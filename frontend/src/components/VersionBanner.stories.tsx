import type { Meta, StoryObj } from '@storybook/react-vite'
import { VersionBanner } from './VersionBanner'

const meta: Meta<typeof VersionBanner> = {
  title: 'App/VersionBanner',
  component: VersionBanner,
}
export default meta

type Story = StoryObj<typeof VersionBanner>

// A different SPA build is served than the one in this tab; the page keeps working.
export const Reload: Story = {
  args: { skew: 'reload' },
  parameters: { renderCheck: { viewports: ['mobile', 'desktop'] } },
}

// This SPA speaks a newer API than the server (UI deployed first, or the backend rolled back); no reload offered.
export const OlderServer: Story = {
  args: { skew: 'older-server' },
  parameters: { renderCheck: { viewports: ['mobile', 'desktop'] } },
}

// The server dropped the API version this build speaks; the page is covered until reloaded.
export const Blocked: Story = {
  args: { skew: 'blocked' },
  parameters: { renderCheck: { viewports: ['mobile', 'desktop'] } },
}
