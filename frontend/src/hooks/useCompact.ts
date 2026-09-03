import { useMediaQuery } from './useMediaQuery'

// Material 3's compact window size class (#1131 epic): 0-599px is
// single-column/touch-first; 600px+ (medium/expanded) adds panes. This is
// the one place that threshold is defined for JS branches that render a
// genuinely different subtree (drawer vs. persistent rail) rather than just
// resizing - CSS-only cases use the matching `medium:` breakpoint added to
// index.css's @theme block instead of this hook.
const COMPACT_QUERY = '(max-width: 599px)'

export function useCompact(): boolean {
  return useMediaQuery(COMPACT_QUERY)
}
