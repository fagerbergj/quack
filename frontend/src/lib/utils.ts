import { clsx, type ClassValue } from 'clsx'
import { extendTailwindMerge } from 'tailwind-merge'

// text-2xs is a theme font size; without this tailwind-merge reads it as a text colour and drops the real one.
const twMerge = extendTailwindMerge({ extend: { classGroups: { 'font-size': [{ text: ['2xs'] }] } } })

export function cn(...inputs: ClassValue[]) {
  return twMerge(clsx(inputs))
}
