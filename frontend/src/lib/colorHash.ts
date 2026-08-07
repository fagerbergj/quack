// Deterministic colour coding (#746 items 10/13): a repo badge or a memory
// pill (bucket/author/kind) gets the same colour every time, derived from its
// own label - not assignment order - so it survives a reload and is
// identical across chats. Colour is always paired with the label text; it
// is never the only signal (WCAG 1.4.1, and this tool's terminal-adjacent
// audience includes colour-blind readers).

// A fixed, named palette - every class below is a literal string so
// Tailwind's scanner generates the CSS; hashPalette only ever SELECTS one of
// these, never constructs a class name at runtime. Each entry pairs a light
// and dark background with a same-hue text colour chosen for contrast.
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
