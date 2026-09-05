import { describe, expect, it } from 'vitest'
import { imageAttachmentsByTurn } from './turnAttachments'
import type { ArtifactList } from '../generated'

describe('imageAttachmentsByTurn', () => {
  it('strips the "bytes:upload-" recordstore id prefix (#1126, #1208) for display/download name', () => {
    const artifacts: ArtifactList = {
      data: [
        {
          name: 'bytes:upload-ui-dialog.png',
          revisions: [{ revision: 1, mime_type: 'image/png', size: 3, turn_id: 't1', created_at: '2026-01-01T00:00:00Z' }],
        },
      ],
    }
    const byTurn = imageAttachmentsByTurn('chat1', artifacts)
    expect(byTurn.t1).toEqual([
      { url: '/api/v1/chats/chat1/artifacts/bytes%3Aupload-ui-dialog.png?revision=1', mime: 'image/png', name: 'ui-dialog.png' },
    ])
  })
})
