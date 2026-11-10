/** A URL slug: lowercase ASCII words joined by single hyphens, with no hyphen at either end. */
export function slugify(s: string): string {
  return s.toLowerCase().trim().replace(/[^a-z0-9]+/g, '-').replace(/^-+|-+$/g, '');
}
