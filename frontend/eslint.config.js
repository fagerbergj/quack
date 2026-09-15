import js from '@eslint/js'
import tseslint from 'typescript-eslint'
import globals from 'globals'

export default tseslint.config(
  { ignores: ['dist', 'src/generated', 'node_modules', 'storybook-static'] },
  js.configs.recommended,
  ...tseslint.configs.recommended,
  {
    languageOptions: {
      globals: { ...globals.browser },
      parserOptions: {
        // Type-aware rules (promise discipline) need the TS project. The root
        // config files sit outside tsconfig's scope; allowDefaultProject
        // parses them without type info (CI only lints src/, which is in scope).
        projectService: {
          allowDefaultProject: [
            '*.js',
            'openapi-ts.config.ts',
            'vitest.render-check.config.ts',
            '.storybook/*.ts',
            'scripts/*.mjs',
          ],
        },
        tsconfigRootDir: import.meta.dirname,
      },
    },
    rules: {
      // Slop gate, same threshold as the Go gate (AGENTS.md, .golangci.yml):
      // branch stacks over 15 decision points fail CI on new/touched code.
      complexity: ['error', 15],
      // Unhandled async: a floating promise or a promise where a void
      // return is expected hides a real failure (the UI moves on while the
      // request dies). Error in app code.
      '@typescript-eslint/no-floating-promises': 'error',
      '@typescript-eslint/no-misused-promises': 'error',
      // The store/components intentionally use a few `any`-ish escape hatches.
      '@typescript-eslint/no-explicit-any': 'off',
    },
  },
  {
    // Legacy ratchet: promise rules in tests and stories, measured 2026-09-15
    // (39 findings across these 10 files - test scaffolding and story fetch
    // stubs that never need the discipline). Warns until the backlog is paid
    // down; every other file is hard error. Remove a file when clean.
    files: [
      'src/components/AgentParts.stories.tsx',
      'src/components/ArtifactPanel.rtl.test.tsx',
      'src/components/ChatMenu.stories.tsx',
      'src/components/ChatMenu.test.ts',
      'src/components/MemorySortFilter.stories.tsx',
      'src/components/MemoryTab.stories.tsx',
      'src/components/MemoryTab.test.ts',
      'src/components/NavRail.stories.tsx',
      'src/components/TriggerEnvelope.test.ts',
      'src/state/chatStore.test.ts',
    ],
    rules: {
      '@typescript-eslint/no-floating-promises': 'warn',
      '@typescript-eslint/no-misused-promises': 'warn',
    },
  },
  {
    files: ['scripts/**/*.mjs'],
    languageOptions: { globals: { ...globals.node } },
  },
)
