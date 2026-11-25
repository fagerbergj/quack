// The report's Markdown style checks, ported from lintdec's default-on md-* rules (github.com/fagerbergj/lintdec, rules.go and veto.go at fdf08ab):
// lintdec's units, found here on the Markdown tree, and its rule and exception wording, asked of Clef as one question per unit.
import type { List, Paragraph } from 'mdast';
import { CONFIDENT_ONLY, registerSplitter, type Chunk, type CheckDecl, type Inputs, type Question, type Scope, type SplitDecl } from '@quack/judge';
import { nodes, parse, plain, source } from './markdown.ts';

const tidy = (s: string) => s.replace(/\s*\[s\d+\]/g, '').replace(/\[([^\]]*)\]\([^)]*\)/g, '$1').replace(/[ \t]+/g, ' ').trim();
const blocks = (report: string) => parse(report).children;
const report = (inputs: Inputs, o: SplitDecl) => inputs[[o.from].flat()[0]];

// A dotted abbreviation or an initial does not end a sentence; fusing two sentences is safer than cutting one in half.
const ABBREVIATION = /(?:\b(?:[A-Za-z]\.){2,}|\b[A-Z]\.|\b(?:etc|vs|cf|approx|Dr|Mr|Mrs|Ms|Jr|Sr|Inc|Ltd|St|Fig|No)\.)$/;

/** A text's sentences: a stop, then space, then a capital, figure or opening quote starts the next. */
export function sentences(text: string): string[] {
  const out: string[] = [];
  for (const piece of text.replace(/\s+/g, ' ').trim().split(/(?<=[.!?]["')\]”’]*)\s+(?=["“(]?[\p{Lu}\p{N}])/u)) {
    if (out.length && ABBREVIATION.test(out.at(-1)!)) out[out.length - 1] += ` ${piece}`;
    else if (piece) out.push(piece);
  }
  return out;
}

const paragraphs = (report: string) => blocks(report).filter((b): b is Paragraph => b.type === 'paragraph');
const paragraphChunks = (text: string, keep: (p: Paragraph) => boolean): Chunk[] =>
  paragraphs(text).flatMap((p, i) => (keep(p) ? [{ id: `paragraph-${i + 1}`, text: tidy(source(text, p)) }] : []));

// Three inline items need two separators, one a comma or semicolon ("a, b and c"; "first a, then b, then c").
const listLike = (s: string) => /[,;]/.test(s) && (s.match(/[,;]|\b(?:and|or|then)\b/g)?.length ?? 0) >= 2;

/** md-prose-list's candidates: each sentence of a top-level paragraph that could hold an inline list, asked on its own. */
function proseListSentences(inputs: Inputs, o: SplitDecl): Chunk[] {
  const all = paragraphs(report(inputs, o)).flatMap((p) => sentences(plain(p)));
  return all.flatMap((t, i) => (listLike(t) ? [{ id: `sentence-${i + 1}`, text: t }] : []));
}

/** The report's opening: everything up to and including its first paragraph or list, as lintdec's md-no-purpose reads a document's title and first paragraph. */
function opening(inputs: Inputs, o: SplitDecl): Chunk[] {
  const text = report(inputs, o);
  const first = blocks(text).find((b) => b.type === 'paragraph' || b.type === 'list');
  const shown = first && tidy(text.slice(0, first.position!.end.offset));
  if (!shown) return [];
  return [{ id: 'opening', text: shown.length > OPENING_CHARS ? `${shown.slice(0, OPENING_CHARS)}\n… (cut at ${OPENING_CHARS} characters)` : shown }];
}
const OPENING_CHARS = 2000;

// lintdec's wordyPhrase list (candidates.go); a found phrase is marked so the question judges only it.
const WORDY = /\b(in order (?:to|for)|due to the fact that|owing to the fact that|at this point in time|at the present time|(?:is|are|was|were) able to|in the event that|a large number of|the majority of|for the (?:sole )?purpose of|in spite of the fact that|despite the fact that|with regard to|in regard to|prior to|subsequent to|has the ability to|have the ability to|make use of|in the near future|on a (?:daily|weekly|monthly|regular) basis|it is important to note that)\b/gi;
// A quotation of four or more words is someone else's wording, not the writer's.
const QUOTED = /["“][^"”]*(?:\s[^"”\s]+){3,}["”]/g;

// Whether a match at `at` is the writer's own replaceable wording: outside inline code (quoted material) and not negated ("not able to" has no one-word swap).
const ownWords = (s: string, at: number) => s.slice(0, at).split('`').length % 2 === 1 && !/(not|n't) $/i.test(s.slice(Math.max(0, at - 6), at));

/** One unit per stock wordy phrase in the writer's own sentences, the phrase marked ⟦like this⟧. */
function wordyPhrases(inputs: Inputs, o: SplitDecl): Chunk[] {
  const text = report(inputs, o);
  const prose = blocks(text).filter((b) => b.type === 'paragraph' || b.type === 'list').flatMap((b) => [...nodes(b)].filter((n) => n.type === 'paragraph'));
  // Quotes go before the split, so one spanning sentences is still found whole.
  const own = prose.flatMap((p) => sentences(plain(p).replace(QUOTED, '…')));
  return own.flatMap((s) => [...s.matchAll(WORDY)].filter((m) => ownWords(s, m.index))
    .map((m) => `${s.slice(0, m.index)}⟦${m[0]}⟧${s.slice(m.index + m[0].length)}`))
    .map((t, i) => ({ id: `phrase-${i + 1}`, text: t }));
}

// lintdec's itemPrefix: a list whose items all open the same way (a link, bold, code, a short "Label:", or plain) is parallel by its labels.
function opens(item: string): string {
  const t = item.trim();
  if (t.startsWith('[')) return 'link';
  if (t.startsWith('**') || t.startsWith('__')) return 'bold';
  if (t.startsWith('`')) return 'code';
  const k = t.indexOf(':');
  return k > 0 && t.slice(0, k).split(/\s+/).length <= 4 ? 'label' : 'plain';
}
// An item's opening words show its grammar and kind, which is what the question weighs; the rest of a long bullet only blurs it.
const ITEM_WORDS = 12;
// An item as opens() judged it: a link keeps its [text] brackets, only its URL and the [sN] marks go, so Clef sees the difference that put the list here.
const opener = (item: string) => item.replace(/\s*\[s\d+\]/g, '').replace(/\[([^\]]*)\]\([^)]*\)/g, '[$1]').replace(/[ \t]+/g, ' ').trim();

/** Each top-level list whose items open in more than one way, as each item's opening words. */
function mixedLists(inputs: Inputs, o: SplitDecl): Chunk[] {
  const text = report(inputs, o);
  const lists = blocks(text).filter((b): b is List => b.type === 'list');
  return lists.flatMap((l, i) => {
    const items = l.children.map((it) => (it.children[0] ? source(text, it.children[0]).split('\n', 1)[0] : ''));
    if (items.length < 2 || new Set(items.map(opens)).size < 2) return [];
    const open = (t: string) => { const w = opener(t).split(' '); return w.length > ITEM_WORDS ? `${w.slice(0, ITEM_WORDS).join(' ')} …` : w.join(' '); };
    return [{ id: `list-${i + 1}`, text: items.map((t) => `- ${open(t)}`).join('\n') }];
  });
}

registerSplitter('research.opening', opening);
registerSplitter('research.proseListSentences', proseListSentences);
// lintdec's exception for md-topic-sentence: a paragraph of fewer than three sentences has no rest for its first to point away from.
registerSplitter('research.longParagraphs', (i, o) => paragraphChunks(report(i, o), (p) => sentences(plain(p)).length >= 3));
registerSplitter('research.wordyPhrases', wordyPhrases);
registerSplitter('research.mixedLists', mixedLists);

const defect = (instructions: string): Question => ({ type: 'bool', instructions, pass: ['no'] });

/** A style check: lintdec's rule as the question, its exception as when to answer no, scored as the share of units without the problem;
 *  as lintdec reports a finding only over its confidence floor, an item Clef is unsure of holds. */
type Style = { id: string; label: string; level: 'warning' | 'info'; scope: Scope; question: Question };

const STYLES: Style[] = [
  { id: 'no_purpose', label: "Opening doesn't say what the report answers", level: 'warning', scope: { split: { name: 'research.opening', from: 'report' }, piece: 'opening' },
    question: defect('Do these opening lines of a research report fail to tell the reader what the report is for or what question it answers, so a reader who reads only them cannot tell whether it is relevant to them? Answer no when the heading or first paragraph tells the reader what the report covers or answers.') },
  { id: 'prose_list', label: 'A list buried in a sentence', level: 'info', scope: { split: { name: 'research.proseListSentences', from: 'report' }, piece: 'sentence' },
    question: defect('Does this sentence pack three or more parallel items, options, or steps inline ("create, update, publish, and delete"; "first X, then Y, then Z") where a bulleted or numbered list would be easier to scan? Answer no when the inline items are short words or names that read naturally in one sentence (such as "Go, Kotlin, and TypeScript"), or the clauses are one argument building to a conclusion.') },
  { id: 'topic_sentence', label: "A paragraph's first sentence points the wrong way", level: 'info', scope: { split: { name: 'research.longParagraphs', from: 'report' }, piece: 'paragraph' },
    question: defect("Does this paragraph's first sentence set up a different subject from the one the rest of the paragraph is about, so a reader who reads only the first sentence gets the wrong idea? Answer no when the first sentence states or leads into the subject of the rest.") },
  { id: 'wordy_phrase', label: 'Several words where one will do', level: 'info', scope: { split: { name: 'research.wordyPhrases', from: 'report' }, piece: 'sentence' },
    question: defect('Is the phrase marked ⟦like this⟧ a stock wordy phrase that one short word replaces with no change of meaning (in order to: to; due to the fact that: because; is able to: can; in the event that: if; prior to: before; for the purpose of: to or for; make use of: use)? Answer no when the one-word replacement would change the meaning here (for example "should be able to" or "for the sole purpose of"), or the phrase is inside quoted text, a title, or code.') },
  { id: 'list_not_parallel', label: "A list's items don't match each other", level: 'info', scope: { split: { name: 'research.mixedLists', from: 'report' }, piece: 'list' },
    question: defect('Do the items of this list fail to match in grammar or kind: some are commands and others noun phrases or full sentences, some active and others passive, or one item belongs to a different category from the rest? Answer no when the items match well enough that a reader would not notice a mismatch, for example all following the same label-then-detail pattern.') },
];

// lintdec's levels: a warning weighs twice an info.
const WEIGHT = { warning: 2, info: 1 };

export const styleChecks = STYLES.map((s) => ({ id: s.id, label: s.label, weight: WEIGHT[s.level] }));
export const styleScopes: Record<string, Scope> = Object.fromEntries(STYLES.map((s) => [s.id, s.scope]));
export const styleDecls: Record<string, CheckDecl> = Object.fromEntries(STYLES.map((s) => [s.id, { scope: s.id, question: s.question, aggregate: { name: 'share' }, decisionModel: CONFIDENT_ONLY }]));
