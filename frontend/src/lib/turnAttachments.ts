import type { ArtifactList } from '../generated'
import type { AttachmentPreview } from '../components/AttachmentUI'

// Maps chat artifacts (where attachment bytes land, see rest/handler.go saveAttachment) to turn_id -> image previews,
// so a persisted turn shows real thumbnails. Non-image artifacts and revisions without a turn_id are skipped.
export function imageAttachmentsByTurn(chatId: string, artifacts: ArtifactList): Record<string, AttachmentPreview[]> {
  const byTurn: Record<string, AttachmentPreview[]> = {}
  for (const artifact of artifacts.data) {
    for (const rev of artifact.revisions) {
      if (!rev.turn_id || !rev.mime_type.startsWith('image/')) continue
      const url = `/api/v1/chats/${encodeURIComponent(chatId)}/artifacts/${encodeURIComponent(artifact.name)}?revision=${rev.revision}`
      // Attachments save as recordstore kind "bytes" with an "upload-" hint, keeping uploads out of the dispatch
      // input-artifact namespace; strip "bytes:upload-" back off for display.
      const ATTACHMENT_PREFIX = 'bytes:upload-'
      const name = artifact.name.startsWith(ATTACHMENT_PREFIX) ? artifact.name.slice(ATTACHMENT_PREFIX.length) : artifact.name
      const list = byTurn[rev.turn_id] ?? (byTurn[rev.turn_id] = [])
      list.push({ url, mime: rev.mime_type, name })
    }
  }
  return byTurn
}
