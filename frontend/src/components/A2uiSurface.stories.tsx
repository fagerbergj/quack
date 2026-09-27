import type { Meta, StoryObj } from '@storybook/react-vite'
import { within, userEvent } from 'storybook/test'
import A2uiSurfaceView from './A2uiSurface'
import { pr1085, pr9 } from './A2uiSurface.fixtures'

const meta: Meta<typeof A2uiSurfaceView> = {
  title: 'Chat/A2uiSurface',
  component: A2uiSurfaceView,
  parameters: { layout: 'padded', renderCheck: { viewports: ['mobile', 'desktop'] } },
}
export default meta

type Story = StoryObj<typeof A2uiSurfaceView>

const openQuiz = async ({ canvasElement }: { canvasElement: HTMLElement }) => {
  await userEvent.click(await within(canvasElement).findByRole('tab', { name: 'Quiz' }))
}

// First render_ui call of a real PR-tutor run: overview, flow, hunks, quiz.
export const Pr1085FirstRender: Story = { args: { content: pr1085.first } }

// The grading upsert on the same surface: a verdict under each question and the score on the button.
export const Pr1085Graded: Story = {
  args: { content: pr1085.graded },
  parameters: { renderCheck: { viewports: ['mobile', 'desktop'], play: true } },
  play: openQuiz,
}

export const Pr9FirstRender: Story = { args: { content: pr9.first } }

export const Pr9Graded: Story = {
  args: { content: pr9.graded },
  parameters: { renderCheck: { viewports: ['mobile', 'desktop'], play: true } },
  play: openQuiz,
}
