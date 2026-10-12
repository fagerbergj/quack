// Shared story wrappers: every story returns real DOM (html-vite accepts a
// string too, but an element lets render-check measure scrollWidth).
export function page(html, maxWidth = '48rem') {
  const el = document.createElement('div')
  el.className = 'qk-page'
  el.style.maxWidth = maxWidth
  el.innerHTML = html
  return el
}

// A fixed-width frame so a story previews at phone width even in a wider canvas; render-check also
// resizes the real viewport.
export function mobileFrame(html) {
  const outer = document.createElement('div')
  outer.className = 'qk-page'
  // No overflow-x:hidden - that would clip the very overflow render-check measures.
  outer.style.cssText = 'width:390px;max-width:390px;border:1px solid var(--qk-border)'
  outer.innerHTML = html
  return outer
}

export function sidebar(html) {
  const el = document.createElement('div')
  el.className = 'qk-page'
  el.style.maxWidth = '20rem'
  el.innerHTML = html
  return el
}
