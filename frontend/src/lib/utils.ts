import { clsx, type ClassValue } from 'clsx'
import { extendTailwindMerge } from 'tailwind-merge'

// text-2xs and shadow-overlay are theme tokens; without this tailwind-merge misreads them as colours and drops the real size or shadow.
const twMerge = extendTailwindMerge({ extend: { classGroups: { 'font-size': [{ text: ['2xs'] }], shadow: [{ shadow: ['overlay'] }] } } })

export function cn(...inputs: ClassValue[]) {
  return twMerge(clsx(inputs))
}
