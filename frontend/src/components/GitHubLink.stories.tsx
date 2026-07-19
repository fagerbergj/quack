import type { Meta, StoryObj } from '@storybook/react-vite'
import { GitHubLink } from './GitHubLink'

const meta: Meta<typeof GitHubLink> = {
  title: 'Chat/GitHubLink',
  component: GitHubLink,
  parameters: { layout: 'padded' },
}
export default meta

type Story = StoryObj<typeof GitHubLink>

// As used in the chat header: repo name + link, next to the chat title.
export const WithRepo: Story = {
  render: () => <GitHubLink url="https://github.com/acme/widgets/issues/7" repo="acme/widgets" />,
}

// Without a repo label, e.g. once the repo is already shown elsewhere.
export const LinkOnly: Story = {
  render: () => <GitHubLink url="https://github.com/acme/widgets/pull/42" />,
}
