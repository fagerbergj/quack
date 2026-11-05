// The six candidates as data: each scope names a chunker and context selectors, each check a question and an aggregator. No candidate has code of its own.
import type { Check, Declaration, Where } from './strategies.ts';

const FAIR = ' Judge only from the text shown; a fact you do not recognise is not evidence of error.';
const CITE = '\\s*\\[s\\d+\\]';
const ASK_LIKE = '\\?|^(?:question:\\s*)?(?:what|how|when|who|where|which|why|is|are|does|compare|cover|list|explain|identify|find|recommend|give|show|rank|include)\\b';
const ASK_SPLIT = ',?\\s+and\\s+(?=(?:what|when|how|who|where|which|is|are|does)\\b)|,\\s*(?=(?:what|when|how|who|where|which|is|are)\\b)|;';

export const research: Declaration = {
  scopes: {
    asks: {
      split: { name: 'sentences', from: 'request', splitOn: ASK_SPLIT, match: ASK_LIKE },
      piece: 'ask',
      context: {
        request: { name: 'whole', from: 'request', maxChars: 800 },
        summary: { name: 'sections', from: 'report', names: ['Summary'], maxChars: 3000 },
        findings: { name: 'related', chunker: { name: 'listItems', from: 'report', sections: ['Findings'], strip: CITE }, match: 'overlap', top: 6, maxChars: 5000 },
        assumptions: { name: 'sections', from: 'report', names: ['Assumptions'], maxChars: 1500 },
      },
    },
    summarySentences: {
      split: { name: 'sentences', from: 'report', sections: ['Summary'], strip: CITE },
      piece: 'statement',
      context: { findings: { name: 'related', chunker: { name: 'listItems', from: 'report', sections: ['Findings'], strip: CITE }, match: 'overlap', top: 8, maxChars: 6000 } },
    },
    factPairs: { split: { name: 'digestPairs', of: { name: 'sentences', from: 'report', strip: CITE }, extract: 'numbers', compare: 'sameTermsOtherValue', minShared: 2, top: 3 } },
  },
  checks: {
    answers_every_ask: { scope: 'asks', question: { type: 'bool', instructions: `Is this ask answered in the report's Summary or Findings, or named under Assumptions as unsettled?${FAIR}` }, aggregate: { name: 'all' }, flagWhen: false },
    summary_follows_findings: { scope: 'summarySentences', question: { type: 'bool', instructions: `Does the statement follow from the findings: every fact in it appears in them?${FAIR}` }, aggregate: { name: 'all' }, flagWhen: false },
    no_self_contradiction: { scope: 'factPairs', question: { type: 'bool', instructions: `Do the two passages state conflicting values for the same thing?${FAIR}` }, aggregate: { name: 'any' }, flagWhen: true },
  },
};

const PROMISE_VERBS = '\\b(adds?|now|fix(?:es)?|supports?|when|replaces?|removes?|makes?|returns?|so that|covered|drops?|stops?|keeps?|uses?|runs?|moves?|cuts?|lets?|allows?|shows?|sets?|resolves?|ignores?|rejects?|answers?)\\b';

export const review: Declaration = {
  scopes: {
    promises: {
      split: { name: 'sentences', from: ['description', 'issues'], skip: ['checked', 'test plan', 'tests', 'verification', 'testing'], strip: '🤖.*|\\*\\*|`', match: PROMISE_VERBS, minWords: 5, max: 5 },
      piece: 'promise',
      context: {
        hunks: { name: 'related', chunker: { name: 'diffHunks', from: 'diffW' }, match: ['names', 'overlap'], top: 3, maxChars: 8000 },
        review: { name: 'sections', from: 'review', names: ['Takeaway', 'Findings'], maxChars: 3000 },
      },
    },
    newFiles: {
      split: { name: 'diffFiles', from: 'diff', filter: 'added' },
      piece: 'newFile',
      context: { sibling: { name: 'related', chunker: { name: 'jsonItems', from: 'repoFiles' }, match: 'sibling', top: 1, require: true, maxChars: 12000 } },
    },
    newFilesReviewed: {
      split: { name: 'diffFiles', from: 'diff', filter: 'added' },
      piece: 'newFile',
      context: {
        sibling: { name: 'related', chunker: { name: 'jsonItems', from: 'repoFiles' }, match: 'sibling', top: 1, require: true, maxChars: 12000 },
        reviewLines: { name: 'related', chunker: { name: 'listItems', from: 'review', sections: ['Findings', 'Verified', 'Notes'] }, match: 'cites', top: 6, maxChars: 4000 },
      },
    },
    files: {
      split: { name: 'diffFiles', from: 'diffW' },
      piece: 'diff',
      context: { reviewLines: { name: 'related', chunker: { name: 'listItems', from: 'review', sections: ['Findings', 'Verified', 'Notes'] }, match: 'cites', top: 6, maxChars: 4000 } },
    },
  },
  checks: {
    promises_handled: {
      scope: 'promises',
      question: {
        type: 'choice',
        instructions: 'Does the change deliver this sentence from the description? The hunks are the parts of the diff that mention it; the review is the reviewer\'s takeaway and findings.',
        options: {
          not_a_promise: 'The sentence promises no behaviour: it describes process, tests, motivation or background.',
          delivered: 'The hunks show the promised behaviour.',
          deferred: 'The text says the behaviour comes later, in a follow-up.',
          reported: 'The review names the behaviour as missing or not delivered.',
          missing: 'A promised behaviour that neither the hunks nor the review show.',
        },
      },
      aggregate: { name: 'all', pass: ['not_a_promise', 'delivered', 'deferred', 'reported'] },
      flagWhen: false,
    },
    sibling_parts: {
      scope: 'newFiles',
      question: { type: 'bool', instructions: 'Does the sibling file have a part (handler, export, field, section or test) that the new file lacks, apart from what belongs only to the new file\'s own purpose?' },
      aggregate: { name: 'any' },
      flagWhen: true,
    },
    sibling_unreported: {
      scope: 'newFilesReviewed',
      records: 'sibling_parts',
      question: { type: 'bool', instructions: "Does the sibling file have a part (handler, export, field, section or test) that the new file lacks and that none of the review lines shown mention, apart from what belongs only to the new file's own purpose?" },
      aggregate: { name: 'any' },
      flagWhen: true,
    },
    covered: {
      scope: 'files',
      question: { type: 'bool', instructions: 'Do the review lines shown say something specific about this file: what was read, run or found in it?' },
      aggregate: { name: 'coverageUnion', by: 'either', min: 1 },
      flagWhen: false,
    },
    unnamed_defect: {
      scope: 'files',
      question: { type: 'bool', instructions: 'Does this diff contain a defect (wrong output, a crash, data loss, a security hole) that the review lines shown do not name?' },
      aggregate: { name: 'any' },
      flagWhen: true,
    },
  },
};

const contractsFor = (documentChunker: string): Declaration => ({
  scopes: {
    guarantees: {
      split: { name: 'jsonItems', from: 'guarantees' },
      piece: 'guarantee',
      context: {
        mechanisms: { name: 'related', chunker: { name: documentChunker, from: 'document' }, match: 'overlap', top: 4, maxChars: 5000 },
        choices: { name: 'related', chunker: { name: 'jsonItems', from: 'choices' }, match: 'overlap', top: 3, maxChars: 3000 },
      },
    },
    choices: {
      split: { name: 'jsonItems', from: 'choices', max: 8 },
      piece: 'choice',
      context: { guide: { name: 'related', chunker: { name: 'markdownSections', from: 'guide' }, match: 'overlap', top: 3, maxChars: 6000 } },
    },
    parts: {
      split: { name: documentChunker, from: 'document', max: 8 },
      piece: 'part',
      context: { intent: { name: 'whole', from: 'intent' } },
    },
  },
  checks: {
    meets_intent: { scope: 'guarantees', question: { type: 'bool', instructions: 'Does the document have a mechanism (a header, a status code, a schema rule) that delivers this guarantee from the intent?' }, aggregate: { name: 'all' }, flagWhen: false },
    follows_guide: { scope: 'choices', question: { type: 'bool', instructions: 'Does this choice agree with the guide sections shown, or, where it departs from a guide rule, does its why say what the person or the code required?' }, aggregate: { name: 'all' }, flagWhen: false },
    adds_unasked: { scope: 'parts', question: { type: 'bool', instructions: 'Is this operation or field something the intent asked for, and not excluded by one of its non-goals?' }, aggregate: { name: 'all' }, flagWhen: false },
  },
});

/** One declaration per document kind; only the chunker over the document differs. */
export const contracts = { rest: contractsFor('openapiOperations'), 'json-schema': contractsFor('schemaProperties') };

// The same two checks under each way of splitting free-form input: deterministic, deterministic with a model fallback, model only, model plus a sweep of what it left.
const askScope = { piece: 'ask', context: research.scopes.asks.context };
const promiseScope = { piece: 'promise', context: review.scopes.promises.context };
const askSplit = { name: 'sentences', from: 'request', splitOn: ASK_SPLIT, match: ASK_LIKE };
const promiseSplit = { name: 'sentences', from: ['description', 'issues'], skip: ['checked', 'test plan', 'tests', 'verification', 'testing'], strip: '🤖.*|\\*\\*|`', match: PROMISE_VERBS, minWords: 5 };
const askModel = { name: 'llm', from: 'request', recorded: 'llm:asks' };
const promiseModel = { name: 'llm', from: ['description', 'issues'], recorded: 'llm:promises' };
const withChecks = (name: string, scopes: string[], base: Check) => Object.fromEntries(scopes.map((s) => [`${name}/${s}`, { ...base, scope: s, wire: s.endsWith('Hybrid') ? `${name}/${s.replace('Hybrid', 'Llm')}` : undefined }]));

export const splitters: Declaration = {
  scopes: {
    asksDet: { ...askScope, split: { ...askSplit } },
    asksAuto: { ...askScope, split: { ...askSplit, expect: { min: 1, max: 5, maxChars: 200 }, fallback: askModel } },
    asksLlm: { ...askScope, split: askModel },
    asksHybrid: { ...askScope, split: { ...askModel, sweep: askSplit } },
    promisesDet: { ...promiseScope, split: { ...promiseSplit, max: 5 } },
    promisesAuto: { ...promiseScope, split: { ...promiseSplit, expect: { min: 1, max: 6, maxChars: 300 }, fallback: promiseModel } },
    promisesLlm: { ...promiseScope, split: promiseModel },
    promisesHybrid: { ...promiseScope, split: { ...promiseModel, sweep: promiseSplit } },
  },
  checks: {
    ...withChecks('answers_every_ask', ['asksDet', 'asksAuto', 'asksLlm', 'asksHybrid'], research.checks.answers_every_ask),
    ...withChecks('promises_handled', ['promisesDet', 'promisesAuto', 'promisesLlm', 'promisesHybrid'], review.checks.promises_handled),
  },
};

// Sleeper cards: each rubric's own per-row and per-card model checks over the card JSON (`card`), one declaration per kind.
// The units' facts (incumbent, baseline, table) are not stored with the card, so a row is judged on the card's own fields.
const rows = (path: string, where?: Where[], context?: Record<string, { name: string } & Record<string, unknown>>) => ({ split: { name: 'jsonItems', from: 'card', path, where }, piece: 'row', context });
const text = (path: string) => ({ split: { name: 'jsonItems', from: 'card', path }, piece: 'text' });
const ask = (scope: string, instructions: string, aggregate: 'all' | 'any'): Check => ({ scope, question: { type: 'bool', instructions }, aggregate: { name: aggregate }, flagWhen: aggregate === 'any' });
const has = (path: string): Where => ({ path, op: 'truthy' });

export const sleeper: Record<string, Declaration> = {
  'sleeper.lineup': {
    scopes: { row: rows('starters'), designated: rows('starters', [has('player.inj')]), close: rows('starters', [has('news')]), changed: rows('starters', [{ path: 'action', op: 'ne', value: 'keep' }]), summary: text('summary') },
    checks: {
      picks_an_action: ask('row', 'Does the row start, sit or keep a player, rather than leave the slot open?', 'all'),
      designation_weighted: ask('designated', 'Does the call treat an injury designation as a play rate, not as in or out?', 'all'),
      close_call_news: ask('close', 'Does `why` reflect the attached `news`, naming the right player for each report?', 'all'),
      matchup_overrides_talent: ask('changed', 'Does this change bench a clearly better healthy player for matchup alone?', 'any'),
      weather_mentioned_unmoved: ask('summary', 'Does the summary mention weather for a call that weather did not move?', 'any'),
    },
  },
  'sleeper.waivers': {
    scopes: {
      row: rows('candidates', undefined, { league: { name: 'json', from: 'card', pick: ['waiver_type', 'faab_left', 'my_priority'] } }),
      close: rows('candidates', [has('news')]),
      summary: text('summary'),
    },
    checks: {
      sized_to_waiver_type: ask('row', "Is the claim sized in this league's own currency: a share of remaining FAAB in a FAAB league, or the queue position spent in a rolling or reverse-standings one?", 'all'),
      ranked_on_usage: ask('row', 'Does the rank rest on role and usage, with trending and ownership counts framed as scarcity?', 'all'),
      bye_coverage: ask('row', 'Does it miss a bye inside the lead time, or count a shared bye as one slot?', 'any'),
      crowd_metric_as_value: ask('row', 'Is a trending or ownership number given as the reason the player is good?', 'any'),
      close_call_news: ask('close', 'Does this claim reflect the attached `news`, naming the right player for each report?', 'all'),
      ordered_fallback_flagged: ask('summary', 'Does it describe claims that share a drop as a mistake or a conflict?', 'any'),
    },
  },
  'sleeper.history': {
    scopes: { row: rows('seasons'), summary: text('summary') },
    checks: {
      aggregated_not_scolded: ask('row', "Is each criticism backed by a pattern across weeks, naming the player, pick or week and the number from this season's figures?", 'all'),
      decision_time_grading: ask('row', 'Is a pre-kickoff call judged on what was knowable then, not on how the game turned out?', 'all'),
      single_week_as_pattern: ask('row', 'Is one week, one bust or one trade offered as evidence of a bad manager?', 'any'),
      luck_process_split: ask('summary', 'Does it separate lineup execution and the draft, which the manager controls, from schedule luck (points against) and player-outcome luck?', 'all'),
      efficiency_as_record: ask('summary', 'Does it equate lineup efficiency with winning or losing?', 'any'),
    },
  },
  'sleeper.trends': {
    scopes: { row: rows('items') },
    checks: {
      conflict_stated: ask('row', 'If news and the injury tag disagree, does the item say so and name the rule it followed?', 'all'),
      original_source_named: ask('row', 'Does `source` name the original source: a reporter, outlet, team or league, not an aggregator?', 'all'),
      aggregator_only: ask('row', 'Is an aggregator the only source named for this item?', 'any'),
    },
  },
  'sleeper.digest': {
    scopes: { row: rows('games'), summary: text('summary') },
    checks: {
      game_flow_from_score: ask('row', 'Does it describe a comeback, collapse or other game flow that only the final score implies?', 'any'),
      story_in_summary: ask('summary', "Does the summary name the week's real story (the margin that mattered, the high score, the blowout) rather than recite scores?", 'all'),
      luck_without_margin: ask('summary', 'Does it call a result lucky without the point margin, or from fewer than about eight close games?', 'any'),
    },
  },
  'sleeper.retro': {
    scopes: { misses: rows('misses'), lessons: rows('lessons'), summary: text('summary') },
    checks: {
      hindsight_called_process: ask('misses', "Is a row with knowable false (variance, no signal before kickoff) blamed on the manager's process?", 'any'),
      lessons_actionable: ask('lessons', 'Is the lesson something the manager can do next week?', 'all'),
      summary_names_swing: ask('summary', 'Does the summary name the single biggest swing?', 'all'),
    },
  },
};

// Research's kept claim checks through the library: one chunk per finding, the cited source's best-matching sentences as context.
export const claims: Declaration = {
  scopes: {
    claims: {
      split: { name: 'listItems', from: 'report', sections: ['Findings'], strip: CITE },
      piece: 'finding',
      context: { source: { name: 'related', chunker: { name: 'sentences', from: 'source' }, match: 'overlap', top: 4, maxChars: 4000 } },
    },
  },
  checks: {
    supported_by_source: { scope: 'claims', question: { type: 'bool', instructions: `Does the fetched text of the source this finding cites state what the finding claims?${FAIR}` }, aggregate: { name: 'all' }, flagWhen: false },
    contradicted_by_source: { scope: 'claims', question: { type: 'bool', instructions: `Does the fetched text of the source this finding cites contradict it?${FAIR}` }, aggregate: { name: 'any' }, flagWhen: true },
  },
};
