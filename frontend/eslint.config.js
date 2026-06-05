import js from '@eslint/js'
import tseslint from 'typescript-eslint'
import globals from 'globals'

export default tseslint.config(
  { ignores: ['dist', 'src/generated', 'node_modules'] },
  js.configs.recommended,
  ...tseslint.configs.recommended,
  {
    languageOptions: {
      globals: { ...globals.browser },
    },
    rules: {
      // The store/components intentionally use a few `any`-ish escape hatches.
      '@typescript-eslint/no-explicit-any': 'off',
    },
  },
)
