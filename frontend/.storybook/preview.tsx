import { useEffect } from 'react'
import type { Preview, Decorator } from '@storybook/react-vite'
import '../src/index.css'

// A toolbar toggle to preview components in light/dark (class-based) mode,
// matching how the app sets `dark` on <html>.
export const globalTypes = {
  theme: {
    description: 'Theme',
    defaultValue: 'light',
    toolbar: {
      icon: 'circlehollow',
      items: ['light', 'dark'],
      dynamicTitle: true,
    },
  },
}

const withTheme: Decorator = (Story, context) => {
  const theme = context.globals.theme as string
  useEffect(() => {
    document.documentElement.classList.toggle('dark', theme === 'dark')
  }, [theme])
  return (
    <div className="p-6 bg-gray-50 dark:bg-gray-900 text-gray-900 dark:text-white min-h-screen">
      <div className="max-w-2xl mx-auto">
        <Story />
      </div>
    </div>
  )
}

export const decorators = [withTheme]

const preview: Preview = {
  parameters: {
    controls: { matchers: { color: /(background|color)$/i, date: /Date$/i } },
  },
}

export default preview
