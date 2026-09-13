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
    },
    rules: {
      // Slop gate, same threshold as the Go gate (AGENTS.md, .golangci.yml):
      // branch stacks over 15 decision points fail CI on new/touched code.
      complexity: ['error', 15],
      // The store/components intentionally use a few `any`-ish escape hatches.
      '@typescript-eslint/no-explicit-any': 'off',
    },
  },
  {
    // Legacy ratchet: the 14 files with pre-existing CC > 15 (19 findings,
    // measured 2026-09-12) warn until the de-slop waves shrink them; every
    // other file - and every file added later - is hard error. Remove a
    // file from this list when its functions are back under 15.
    files: [
      'src/components/ArtifactPanel.stories.tsx',
      'src/components/ArtifactPanel.tsx',
      'src/components/ChatList.tsx',
      'src/components/Composer.tsx',
      'src/components/DagNode.tsx',
      'src/components/DagView.stories.tsx',
      'src/components/MemoryEntry.tsx',
      'src/components/MemoryTab.tsx',
      'src/components/NodePopup.tsx',
      'src/components/ToolCallView.tsx',
      'src/components/TurnView.tsx',
      'src/components/envelope.ts',
      'src/pages/Chat.tsx',
      'src/state/agentStream.ts',
    ],
    rules: { complexity: ['warn', 15] },
  },
  {
    files: ['scripts/**/*.mjs'],
    languageOptions: { globals: { ...globals.node } },
  },
)
