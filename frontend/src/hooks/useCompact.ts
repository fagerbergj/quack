import { useMediaQuery } from './useMediaQuery'

// #1174's compact breakpoint: below the 600px medium/expanded line
// (index.css --breakpoint-medium), Composer swaps to the single-pill layout.
export function useCompact(): boolean {
  return useMediaQuery('(max-width: 599px)')
}
