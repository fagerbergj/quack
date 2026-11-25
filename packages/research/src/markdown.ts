import type { Nodes, Root } from 'mdast';
import { fromMarkdown } from 'mdast-util-from-markdown';
import { gfmFromMarkdown } from 'mdast-util-gfm';
import { gfm } from 'micromark-extension-gfm';

/** The report as a GFM syntax tree: bare URLs become links, and code blocks and inline code are their own nodes. */
export const parse = (text: string): Root => fromMarkdown(text, { extensions: [gfm()], mdastExtensions: [gfmFromMarkdown()] });

/** Every node of the tree, depth first. */
export function* nodes(n: Nodes): Generator<Nodes> {
  yield n;
  if ('children' in n) for (const c of n.children) yield* nodes(c);
}

/** A node's words with markup dropped, inline code kept in backticks; citation marks are the report's bookkeeping, not prose. */
export const plain = (n: Nodes): string => (n.type === 'inlineCode' ? `\`${n.value}\`` : 'value' in n ? n.value : 'children' in n ? n.children.map(plain).join('') : '').replace(/\s*\[s\d+\]/g, '');

/** The Markdown source of a node. */
export const source = (text: string, n: Nodes) => text.slice(n.position!.start.offset, n.position!.end.offset);
