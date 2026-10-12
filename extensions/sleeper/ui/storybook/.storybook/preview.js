// kit.css is vendored from quack/frontend/public/assets/ext/v1/kit.css (separate repo; v1 is
// frozen-additive). page.css is imported from its served location so it can't drift.
import './kit.css'
import '../../static/page.css'
import { installAvatarFallback } from '../../static/render.js'

installAvatarFallback()

export const globalTypes = {
  theme: {
    description: 'Theme',
    defaultValue: 'light',
    toolbar: { icon: 'circlehollow', items: ['light', 'dark'], dynamicTitle: true },
  },
}

// Like quack/frontend's withTheme decorator but stamps data-theme for page.css's :root[data-theme]
// blocks. Stories wrap their own .qk-page, so wrapping here would fight the mobile-frame decorator.
const withTheme = (storyFn, context) => {
  document.documentElement.dataset.theme = context.globals.theme || 'light'
  return storyFn()
}

export const decorators = [withTheme]

export default {
  parameters: { layout: 'fullscreen' },
}
