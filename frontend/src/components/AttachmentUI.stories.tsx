import { useState } from 'react'
import type { Meta, StoryObj } from '@storybook/react-vite'
import { AttachmentPreviews, AttachmentStrip, type AttachmentItem } from './AttachmentUI'

// ── AttachmentPreviews ────────────────────────────────────────────────────────

const previewMeta: Meta<typeof AttachmentPreviews> = {
  title: 'Chat/AttachmentPreviews',
  component: AttachmentPreviews,
}
export default previewMeta

type PreviewStory = StoryObj<typeof AttachmentPreviews>

const IMG_URL = 'https://picsum.photos/seed/quack/200/150'

export const ImageAttachment: PreviewStory = {
  args: {
    previews: [
      { url: IMG_URL, mime: 'image/jpeg', name: 'photo.jpg' },
    ],
  },
}

export const AudioAttachment: PreviewStory = {
  args: {
    previews: [
      { url: '', mime: 'audio/mpeg', name: 'recording.mp3' },
    ],
  },
}

export const Mixed: PreviewStory = {
  args: {
    previews: [
      { url: IMG_URL, mime: 'image/jpeg', name: 'photo.jpg' },
      { url: '', mime: 'audio/wav', name: 'clip.wav' },
      { url: IMG_URL, mime: 'image/png', name: 'screenshot.png' },
    ],
  },
}

export const Empty: PreviewStory = {
  args: { previews: [] },
}

// ── AttachmentStrip ───────────────────────────────────────────────────────────
// Rendered with interactive state so add/remove can be exercised in Storybook.

function StripWrapper({ initial }: { initial: AttachmentItem[] }) {
  const [attachments, setAttachments] = useState<AttachmentItem[]>(initial)
  return (
    <div className="space-y-3">
      <AttachmentStrip
        attachments={attachments}
        onRemove={i => setAttachments(prev => prev.filter((_, j) => j !== i))}
      />
      <p className="text-xs text-gray-400">{attachments.length} attachment(s) staged</p>
    </div>
  )
}

export const StripWithImage: StoryObj = {
  render: () => (
    <StripWrapper initial={[
      { file: new File([''], 'photo.jpg', { type: 'image/jpeg' }), url: IMG_URL },
    ]} />
  ),
}

export const StripWithAudio: StoryObj = {
  render: () => (
    <StripWrapper initial={[
      { file: new File([''], 'clip.mp3', { type: 'audio/mpeg' }), url: '' },
    ]} />
  ),
}

export const StripMultiple: StoryObj = {
  render: () => (
    <StripWrapper initial={[
      { file: new File([''], 'photo.jpg', { type: 'image/jpeg' }), url: IMG_URL },
      { file: new File([''], 'recording.wav', { type: 'audio/wav' }), url: '' },
    ]} />
  ),
}

export const StripEmpty: StoryObj = {
  render: () => <StripWrapper initial={[]} />,
}
