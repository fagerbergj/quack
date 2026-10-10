// @vitest-environment jsdom
import { describe, it, expect, afterEach } from 'vitest'
import { cleanup, render, screen } from '@testing-library/react'
import { TriggerMessage } from './TriggerEnvelope'
import { AttachmentPreviews } from './AttachmentUI'

afterEach(cleanup)

// memo's shallow compare means only reference equality (Chat.tsx's useMemo) may skip a re-render;
// same-length, different-content swaps are what a naive comparator gets wrong.
describe('TriggerMessage attachments prop', () => {
  it('re-renders and shows new attachments when the prop changes, even at the same length', () => {
    const { rerender } = render(
      <TriggerMessage content="plain message" attachments={<AttachmentPreviews previews={[{ url: 'a.png', mime: 'image/png', name: 'first' }]} />} />,
    )
    expect(screen.getAllByAltText('first').length).toBeGreaterThan(0)

    rerender(
      <TriggerMessage content="plain message" attachments={<AttachmentPreviews previews={[{ url: 'b.png', mime: 'image/png', name: 'second' }]} />} />,
    )
    expect(screen.queryAllByAltText('first')).toHaveLength(0)
    expect(screen.getAllByAltText('second').length).toBeGreaterThan(0)
  })
})
