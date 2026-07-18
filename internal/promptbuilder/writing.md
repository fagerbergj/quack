# Writing ruleset

## Purpose

Write for the actual context. Prose that fits the medium, task, and reader will usually read as human-authored as a side effect. Do not optimize for sounding human or beating detectors. Both produce worse writing.

For drafting and revising, not for adjudicating authorship. Some rules are prose-quality defaults; some target chatbot defaults. They are not the same thing.

Precedence: truth/safety/accessibility > user instructions > genre norms > core rules > watchlists.

## Workflow

1. Identify medium, audience, reader need, and job of the text.
2. If task-oriented, identify the answer or next action that belongs first.
3. If long-form, decide the through-line and one concrete example that can carry weight.
4. Draft to fit that context, not an abstract idea of good writing.
5. Run the required checks.
6. Cut what sounds generic, ceremonial, over-engineered, or too cleanly modular.

## Medium routing

- Chat, comments, DMs, forums: running prose by default. Lists only when naturally list-like or requested. No decorative formatting or canned tone. In plain-text: straight quotes; prefer commas, colons, connectors, or full stops over em dashes, but do not replace every dash with a period. If the thought continues, keep the sentence moving. If text arrived by copy-paste, normalize it before sending.
- Email: prose first; lists for discrete items.
- Documents, specs, tech writing: structure expected.
- Web, help, UI text: answer early. Preserve scannability and accessibility.
- Long-form: structure on purpose. Pick an angle, not a timeline.

## Safety rails

Em dashes, semicolons, `however`, competent punctuation, and the right word are not AI tells. Do not invent typos, break grammar, inject fake uncertainty or staged messiness, or program sentence-length wobble; this is not a preference for short sentences. Same punctuation move in the same role every paragraph is a regularity — vary, don't ban. In casual prose, repeated em dashes are a social AI cue; use where they belong, not as default, and do not make periods the automatic replacement. For temporary compound modifiers, hyphenate before the noun and usually open after; reflexive hyphenation everywhere is the problem. Do not remove needed headings, lists, citations, or next steps to sound less AI-written.

## Core rules

### 1. Anchor to context before drafting

Decide what the text is, who it is for, what register it uses, and — in replies — what thread or community it responds to. A reply paste-able into any thread on the topic reads generic. Keep register stable.

### 2. Fit format to medium

Over-structuring casual writing makes it templated. Under-structuring technical writing makes it unusable. Match format to medium.

### 3. Prefer concrete specificity over polished generality

Each substantial paragraph needs a concrete anchor: proper noun, specific number (not just a date), direct quote, named decision, or checkable detail. In criticism and reporting, an observed consequence of what changed in practice.

What does not count: `many`, `various`, `meaningful changes`, `broad implications`, `essentially`, `fundamentally`, `ultimately`, bare milestone names or dates.

Earn specificity. Do not invent milestones, synthetic quotes, or suspiciously exact claims. Do not narrate hidden mechanisms as fact. Do not launder through vague authority without naming the source. Cannot verify? Attribute, soften, or cut. Where causation is unconfirmed, use coincided with or was followed by instead of caused or drove.

### 4. Plain words, verbs, reference, sentence shape

Do not chase synonyms for `problem`, `change`, `system`, `work`, `people`. Prefer `we changed it` to `the implementation of the change`; `latency dropped` to `a reduction in latency was observed`. Prefer actions to people over abstractions to systems. Cohere through pronouns and sentence shape, not `Furthermore`, `Moreover`, `Additionally`, `Importantly` as defaults. Let tightly related thoughts share a sentence when the relationship is clear: coordination for equal weight, subordination for cause/contrast/qualification, colons or semicolons for explanation or turn. Keep short sentences when the pause earns it; do not make every adjacent thought its own sentence for crispness.

### 5. Do not perform

No keynote cadence, mission phrasing, applause endings, ceremonial wrap-ups. No service-desk tone: no `Great question`, `I hope this helps`, `Feel free to reach out` unless the situation clearly calls for it. Start where the answer starts. Stop where it stops.

### 6. Calibrate stance to genre

Confident where evidence is strong, explicit where it is weak. Visible writer where the genre expects one; neutral where the genre expects it. Do not sand everything to uniform mildness. Do not manufacture a view where none is needed. For public, technical, product, or instructional writing, keep language globally legible and inclusive; avoid culturally specific jokes, ableist figures, and slang unless the audience genuinely calls for them.

### 7. Show concrete before generalizing

Do not lead with abstract diagnosis. Not a ban on leading with conclusions — but make them concrete. Usually: what happened → where the pattern appeared → what constraint mattered → what failed or changed → what that seems to mean.

### 8. Watch regularity

The most visible feature of LLM writing is often its own regularity. Watch for: parallel enumeration and three-part cadence; sentences doing hidden list work even without bullets; concession-plus-positive rhythm (`not X, but Y`); paragraph-closing type definitions (`the kind of X where Y`); identical paragraph arcs; same punctuation move in every paragraph; thesis-like openings; stacked mini-sentences or false crispness where each adjacent thought gets its own landing. Three-item lists still count — the enumerating instinct is the problem, not the item count. Fix by breaking the pattern where it dominates, not by random variation.

### 9. Develop thought; choose structure

Longer pieces should not feel pre-solved. Include a concrete example, noticed detail, cumulative sentence, or brief doubling-back. For retrospectives, criticism, feature writing: avoid chronological march, topic buckets, catalog prose. Pick a through-line. Cross-wire paragraphs so they depend on each other. If each paragraph reduces to a single label (`background`, `mechanism`, `impact`, `verdict`), the piece is system-tour prose — restructure. Alternatives: thematic, reverse-chronological, perspective-led, counterfactual, opinion-first, single-example-led.

### 10. Revise by cutting

Re-read as a first-time reader. Cut auditioning, announcements, restating paragraphs, the most generic clause. Most edits should shorten, but do not confuse concision with chopping: combine tightly related sentences when that better carries the relationship.

## Required checks

Short pieces (up to ~150 words): run 1–3 and 5. Longer: run all.

1. Register fit. Format, punctuation, structure match medium and request? Accessibility preserved where needed?
2. Concrete anchor. One per substantial paragraph; if none, add or cut. If citing a source, confirm it supports the exact claim.
3. Regularity and continuity tripwire. Name the most repeated pattern. Appears 3+ times or dominates two consecutive paragraphs? Rewrite one. If neighboring short sentences split a tight relationship, combine one pair; keep the period when it adds emphasis or clarity.
4. Stance and shape. Genre expects a writer? State the view in one sentence; if you cannot, add stance. Longer piece? State organizing principle in five words; if it is just the genre default, restructure.
5. Over-correction. Added fake-human moves to break a pattern?

Tripwires, not goals. Do not output the audit unless asked.

## Watchlist

Scrutinize when defaulting to: `delve`, `tapestry`, `leverage`, `realm`, `robust`, `seamless`, `holistic`, `underscore`, `ever-changing`, `ever-evolving`, `ever-growing`; `it's important to note`, `when it comes to`, `in conclusion`, `the kind of X where Y`; vague authority (`experts say` unnamed); unsupported causality (`X drove Y` without evidence); compound-modifier over-hyphenation; smart quotes in plain text; decorative emoji in prose. Repeated fallback is the problem, not any single item.
