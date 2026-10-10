// Deterministic colour from a label (repo badge, memory pill), so it survives reloads and matches across chats.
// Colour always pairs with the label text, never the only signal (WCAG 1.4.1).

// Every class is a literal string so Tailwind's scanner emits it; hashPalette only selects, never builds class names.
// Each entry pairs light/dark backgrounds with a same-hue, contrasting text colour.
const PALETTE = [
  'bg-red-100 text-red-700 dark:bg-red-900/40 dark:text-red-300',
  'bg-orange-100 text-orange-700 dark:bg-orange-900/40 dark:text-orange-300',
  'bg-amber-100 text-amber-700 dark:bg-amber-900/40 dark:text-amber-300',
  'bg-lime-100 text-lime-700 dark:bg-lime-900/40 dark:text-lime-300',
  'bg-emerald-100 text-emerald-700 dark:bg-emerald-900/40 dark:text-emerald-300',
  'bg-teal-100 text-teal-700 dark:bg-teal-900/40 dark:text-teal-300',
  'bg-cyan-100 text-cyan-700 dark:bg-cyan-900/40 dark:text-cyan-300',
  'bg-indigo-100 text-indigo-700 dark:bg-indigo-900/40 dark:text-indigo-300',
  'bg-purple-100 text-purple-700 dark:bg-purple-900/40 dark:text-purple-300',
  'bg-pink-100 text-pink-700 dark:bg-pink-900/40 dark:text-pink-300',
] as const

// Fixed tones for a named state rather than a hashed label; pair each with text or an icon.
export const TONE = {
  green: 'bg-green-100 text-green-700 dark:bg-green-900/40 dark:text-green-400',
  red: 'bg-red-100 text-red-600 dark:bg-red-900/30 dark:text-red-400',
  purple: 'bg-purple-100 text-purple-700 dark:bg-purple-900/40 dark:text-purple-400',
  yellow: 'bg-yellow-100 text-yellow-700 dark:bg-yellow-900/30 dark:text-yellow-500',
  gray: 'bg-gray-100 text-gray-500 dark:bg-gray-700 dark:text-gray-300',
} as const

// hashString is a small, deterministic (non-cryptographic) string hash - the
// same input always maps to the same output, across reloads and sessions.
export function hashString(s: string): number {
  let h = 0
  for (let i = 0; i < s.length; i++) h = (h * 31 + s.charCodeAt(i)) | 0
  return h >>> 0
}

// paletteClasses derives a stable Tailwind bg/text class pair from a seed
// (repo full name, memory bucket, author, kind, ...).
export function paletteClasses(seed: string): string {
  return PALETTE[hashString(seed) % PALETTE.length]
}
