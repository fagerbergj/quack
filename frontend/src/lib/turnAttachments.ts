import type { ArtifactList } from '../generated'
import type { AttachmentPreview } from '../components/AttachmentUI'

// imageAttachmentsByTurn (#1138) turns the chat's artifact list - the durable
// store attachment bytes actually land in (see internal/server/rest/handler.go's
// saveAttachment) - into a turn_id -> image previews map, so a persisted turn
// can render real thumbnails instead of only the "[User attached: ...]" text
// placeholder. Non-image artifacts (node outputs, etc.) and revisions with no
// turn_id (can't be attributed to a message) are skipped.
export function imageAttachmentsByTurn(chatId: string, artifacts: ArtifactList): Record<string, AttachmentPreview[]> {
  const byTurn: Record<string, AttachmentPreview[]> = {}
  for (const artifact of artifacts.data) {
    for (const rev of artifact.revisions) {
      if (!rev.turn_id || !rev.mime_type.startsWith('image/')) continue
      const url = `/api/v1/chats/${encodeURIComponent(chatId)}/artifacts/${encodeURIComponent(artifact.name)}?revision=${rev.revision}`
      const list = byTurn[rev.turn_id] ?? (byTurn[rev.turn_id] = [])
      list.push({ url, mime: rev.mime_type, name: artifact.name })
    }
  }
  return byTurn
}
