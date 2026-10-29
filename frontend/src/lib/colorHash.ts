import type { BadgeTone } from '../components/ui/badge'

// Deterministic colour from a label (repo badge, memory pill), so it survives reloads and matches across chats.
// Colour always pairs with the label text, never the only signal (WCAG 1.4.1).

// The ten hues a hashed label can take, each a Badge tone (light/dark pairs live there).
const PALETTE: readonly BadgeTone[] = ['red', 'orange', 'amber', 'lime', 'emerald', 'teal', 'cyan', 'indigo', 'purple', 'pink']

// hashString is a small, deterministic (non-cryptographic) string hash - the
// same input always maps to the same output, across reloads and sessions.
export function hashString(s: string): number {
  let h = 0
  for (let i = 0; i < s.length; i++) h = (h * 31 + s.charCodeAt(i)) | 0
  return h >>> 0
}

// paletteTone derives a stable Badge tone from a seed (repo full name, memory bucket, author, kind, ...).
export function paletteTone(seed: string): BadgeTone {
  return PALETTE[hashString(seed) % PALETTE.length]
}
