# Audit: Sleeper agents, skills and workflows

- **Sources:** v0 `extensions/sleeper/plugin` on `main`: 7 agents (prompt, rubric, `agent.yaml`, card), 7 skills, 10 workflows, `plugin.json`; and the Go tools in `extensions/sleeper`
- **Audited:** 2026-11-01 ([#1861](https://github.com/fagerbergj/quack/issues/1861)), before anything ports. Nothing was run and no model was called.
- **v1 side:** [Sleeper requirements](../extensions/sleeper.md) (SL-1..SL-9), [Sleeper RFC](../rfc/extensions/sleeper.md), [schedules RFC](../rfc/extensions/schedules.md) (SCH-1, SCH-4), [judge RFC](../rfc/extensions/judge.md), and the reviewer rubric in `v1/packages/github/src/rubric.ts` as the house style for checks.
- **Outcome:** port five agents and five skills trimmed, merge one skill into another, drop the draft agent and skill and the season-notes job. Fix five contradictions before anything ports (below). Most of the cuts come from moving fetch-and-compute lines into workflow code, which D35 already requires.

## What the audit found

**Size.** The plugin is about 6,200 words of agent prompts, 7,000 of `SKILL.md`, 9,600 of rubrics, 14,500 of skill references, and 58,000 of `references/report.md` research dumps (one per skill, marked "load for a hard case"). The `report.md` files are never loaded by default and are not ported. The seven `research-r*.md` files every skill names as its source do not exist in the repo at all.

**Effectiveness evidence is thin and mostly about the tools.** The repo has no Sleeper evaluation doc, no benchmark, and no recorded outcome for any suggestion. What exists is in commit messages and the extension README:

| Evidence | Source | What it shows |
| --- | --- | --- |
| Week-8 digest hand-summed Sleeper's projections and got every team total wrong (130.4 against 102.44); the judge scored it 0 | #1816 | The digest prompt invited arithmetic the tool should do. Fixed by withholding projections and computing `lineup_avg` in code. |
| 3x replay of seven prod rounds: two trade-analyst criteria unstable (sentence counting 0.33-0.67; news criterion 0.00-1.00). After moving brevity to a schema cap and link provenance to code: 7 criterion cells, 0 unstable (was 2 of 8) | #1722 | Judge-scored brevity and provenance are noise; code checks are stable. |
| Lineup weather criterion scored the same round differently across replays; the code-owned replacement only verified the agent's own `close` flags and was removed | #1730, #1781 | Same lesson, plus a warning that a code check on self-reported bookkeeping checks nothing. |
| trend-scout v5 prompt, 12-item Langfuse A/B: output tokens -21%, wall time -19%, rejected tool calls 7 to 4, judged quality within noise | #1726 | The only prompt A/B in the history. It shows a shorter prompt held quality. |
| Clef stat-line ranges, 2026 weeks 5-7, 210 starters, no news: picked the higher scorer of same-position pairs 59% of the time against the projection's 63%; ran 2-3 points high for RBs and WRs; 73% of actual scores fell inside p10-p90. On 18 same-team pairs: 50% right against the projection's 67% | `extensions/sleeper/README.md` | Predictions do not yet beat projections, which is why SL-6 keeps them observe-only. |
| Four observe-only decision points (`lineup_change`, `waiver_pickup`, `waiver_priority`, `trade_accept`) shadow the weekly calls for later scoring against real points | README, `decisions.go` | No scored result is recorded in the repo. Whether the agents' calls beat a baseline is unmeasured. |

Where a table row below says "none", there is no evidence in the repo for it.

## Contradictions to fix before porting

1. **Projected win totals (the known defect), confirmed.** `matchup-recap/SKILL.md` step 3 still says to publish a playoff line using "Pythagorean expectation, regression toward .500, and the 6th-largest-projected-win-total cutoff". Its reference `playoff-odds-from-records.md` steps 3-5 build `E(W)` from "that specific opponent's projected points for/against" and publish `P(make playoffs)` as a normal-CDF probability. #1816 added to the league-reporter prompt: "you never cite one, a projected total, or a win probability from any other source either", and the tools now return no projections in a digest chat, so the method's inputs no longer exist there. Three more places still assume the old behavior: `recap-structure.md` holds up ESPN's "projected score ... O/U and win probability" block as the template, the reporter rubric's `luck_and_playoff_line_computed` still demands a stated playoff-line method, and `sleeper_standings` already returns `in_playoff_line` (rank within `playoff_teams`) and `remaining_weeks`, which is all a recap needs.
2. **Three definitions of reach and steal.** `history-grading` step 1 tells the analyst to declare PFF's bands (20+ spots, 100+ overall) as the default. `sleeper_draft` returns `adp_verdict` at an 8-pick band and both draft and history prompts forbid recomputing it. `sleeper_history` returns its own `reach`/`steal`/`as_expected` at a 5-rank gap between drafted-as and finish rank. The prompts also say `fair`, the tool says `as_expected`. One definition must live in code, and the skill must state only that one.
3. **Who wins when Sleeper and the news disagree.** trend-scout's prompt and `injury-and-news` step 6 say "Sleeper wins". Step 3 of the same skill says to weigh by source tier, "not by recency". The lineup, waiver and trade prompts and skills say Sleeper's fields "lag" beat-writer news and send the agent to the web precisely because of that. A fact Sleeper marks Questionable and a beat reporter reports Out gets opposite handling depending on the agent.
4. **A league format that does not exist.** `recap-structure.md` rule S2 tells the reporter to state "10 teams, full PPR, rolling waivers, 100 FAAB, 6 playoff spots". The `trade-evaluation` and `draft-strategy` headers repeat "rolling waivers, 100 FAAB". `waivers/SKILL.md` spends about 250 words saying that combination is not a Sleeper setting and that a rolling league's `waiver_budget: 100` must be ignored. Its own `report.md` still frames the league as the hybrid. Every skill also hard-codes 10 teams and the slot list, while the tools return the real `roster_positions` and settings.
5. **The lineup prompt against SL-6.** Lineup-analyst takes `floor`, `ceiling` and `confidence` from `sleeper_predict` and says to "start the player that pair favors". SL-6 requires predictions to stay observe-only (shown, not used in advice) until they beat projections, and the measured 59% against 63% says they do not. In observe mode the tool returns only a `note`, so the paragraph is dead today and would be wrong the moment someone turns decide mode on.

Smaller items, all to settle in the port:

- `start-sit` and `injury-and-news` give the Questionable and Doubtful play rates in two tables with different framing (6.9% and 1.25% by population against a "1%-25% depending on study" range).
- `keep-it-short.md` and the reporter rubric's `concise_and_concrete` score a "200-500 word league-chat digest" with a 40-60 word lede, but the digest is a structured card whose only prose is a one-line `summary`.
- `history-grading` luck uses close-loss buckets of under 5 and under 10 points; the tool flags only under 10 (the weekly points to derive the rest are in its output).
- The trade-analyst prompt asks for a web search per player on every finder suggestion, and waiver-scout reads every roster by tool call. Both sit against the 3-minute on-demand p90 (SL-9) unless code does the fetching.

## Per-item decisions

Words are from the files on `main`. "Env" is the 96-word "Player environment" paragraph copied into five prompts (and a 57-word variant in two more), which restates what the tools' `envDoc` already says about their own fields. "Boilerplate" is the "You do not name the artifact yourself" paragraph (30 words) plus the closing "reply with a short markdown summary ... the artifact is the deliverable" (about 45), present in all seven.

### Agents

| Agent | Effectiveness evidence | Alignment with v1 | Conciseness | Decision |
| --- | --- | --- | --- | --- |
| lineup-analyst | Most-iterated prompt (#1730, #1781, #1817, #1819). The weather criterion was unstable under replay. No measure that its start/sit calls beat the roster's `best_by_projection`; the `lineup_change` decision point is unscored in the repo. | Live (SL-1 lineup). The `sleeper_predict` section is contradicted by SL-6. Lock handling and the projection baseline are code in v1 (D35). | 1,235 words; Output alone is 757. Cut: the predict paragraph (~190), env, the field-by-field schema walkthrough (the schema and its caps carry it), the "lineup given as context" sentence (also in the skill), `current_date` call, boilerplate. | Port trimmed. Keep: decide every slot including FLEX, name the baseline player a changed slot beats, one picked action, close-call news. |
| waiver-scout | Large rubric (2,303 words, 12 criteria) suggests many past failures, none recorded. #1817 fixed it reading free-agent injury rows. `waiver_pickup`/`waiver_priority` points unscored. | Live (SL-1 waivers). Reading every roster and the waiver-type warnings become workflow code and tool data (below). | 982 words. Cut: the 142-word Skill section (two of its three paragraphs restate the skill), the ordered-claims paragraph (in skill and rubric too), the `waiver_budget` warning, env, tool-by-tool restatement. | Port trimmed. The workflow fetches all rosters and passes one league-competition table; the model ranks, sizes and writes. |
| trade-analyst | #1722: 3x replay found its two unstable criteria; both moved to code. Nothing on whether `send/counter/decline` match later outcomes (`trade_accept` unscored). | Live (SL-1 finder, analysis, trade talk). Covers two jobs with different inputs; v1 lists three. | 1,065 words. The two job sections run 337 and 344 and repeat the skill's partner-modeling step. Cut: the partner-model paragraph (code builds the contender/rebuilder and surplus/need table), tool restatement, env, boilerplate. | Port trimmed and split: finder, and analysis with trade talk. |
| league-reporter | #1816: the week-8 digest error and judge score of 0. No evidence for the retro prompt, which is 365 of 989 words. | Live (SL-1 league report). Digest and retro have different data and cards. The playoff-line guidance is contradicted (item 1). | 989 words. Cut: the retro's `swaps`/`best_lineup` field-copying instructions (about 150 words; code writes `best`, `left`, `misses`, `waiver_misses` from the tool, the model writes `why`, `lessons`, `summary`), the digest's number-copying, env, boilerplate. | Port trimmed and split into digest and retro. |
| history-analyst | None. The only prompt change since the move to the monorepo is #1817's environment paragraph; the tool computes everything the card reports. | Live (SL-1 league history). The prompt already says "never recompute", which means the card's numbers are code in v1. | 515 words. Cut: tools section (145, restates the tool), env, `current_date`, boilerplate. What is left is "write the narrative from these figures". | Port trimmed. Code assembles the numbers; the model writes the net-position story. |
| trend-scout | #1726 A/B: v5 cut tokens 21% and time 19% at equal judged quality. The only agent with a measured prompt change. | Live (SL-1 player trends). Its second job, season notes, has no v1 home (below). Contradiction 3. | 777 words. Cut: season-notes procedure (about 150), the DEF-as-team-code note (the tool labels `position: "DEF"`; the sentence appears three times), env, boilerplate. | Port trimmed. Trends only. |
| draft-analyst | None. Not mentioned in any commit message after the move. | Dead: SL-1 and S1 list no draft job. | 640 words. | Drop. If drafts return, start from the `adp_verdict` code, not the prompt. |

### Skills

| Skill | Effectiveness evidence | Alignment with v1 | Conciseness | Decision |
| --- | --- | --- | --- | --- |
| start-sit | Its weather and lock rules caused the churn in #1730 and #1781. The thresholds table cites sources with dates; nothing measures that the rules help. | Live. Step 3 (locks, about 330 words) and step 5's call mechanics (about 250) restate the `game` join and roof fixes that code does. Steps 7-8 are contradicted by SL-6. | 1,524 words, plus 1,198 in references. Cut: steps 3, 7, 8 and the call mechanics, the `best_by_projection` paragraph (duplicated in the prompt), the dangling "Source: research-r1-startsit.md". | Port trimmed (~700 words): availability first, close-call news, bounded matchup and weather, FLEX last, the one threshold table (now holding `injury-and-news`'s rows too), "no published number". |
| injury-and-news | #1726 is the only evidence, and it concerns the agent prompt. | Live for trends, but contradicts itself on source conflicts (item 3). Its play-rate table duplicates start-sit's. | 731 words, plus 1,142. Cut: the threshold table (moves to start-sit), the DEF sentence, "bad advice" list (150 words; belongs in the rubric). | Merge the table into start-sit; port the rest trimmed as the trends skill: source ranking, dating, 90-minute inactive rule, false signals. |
| waivers | None recorded. The "Ordered claims ... never an error" section reads as a patched failure; no commit says which. | Live. The type table and hybrid warning describe a tool field that should simply be empty unless `waiver_type` is `faab`. Steps 1, 5 and 6 are fetch order, i.e. code. | 1,596 words, plus 1,649. Cut: the type table, hybrid warning and misleading `report.md` framing (about 250), "bad advice" list (moves to the rubric), steps 1, 5, 6 as procedure. | Port trimmed (~600): bucket candidates, model add-minus-drop, trending is scarcity not value, close-call news, K/DEF and bye rules. |
| trade-evaluation | #1722 only. | Live. Header hard-codes the wrong league (contradiction 4). Steps 2 and 3 duplicate the prompt and rubric. | 1,015 words, plus 3,216 in references (largest set). Cut: header league line, steps 2-3, step 8's write-up rules that the schema's 350-character cap replaces. | Port trimmed. Keep: chart mismatch, scarcity, 2-for-1 discount, roster fit, fairness, "no published number". |
| matchup-recap | #1816 only. | Live for digest and retro, with contradiction 1. Step 2 (verify lineup legality) is what `swaps` and `free_agent_hits` guarantee in code. `recap-structure` and `keep-it-short` assume prose. | 731 words, plus 2,089. Cut: step 3 and `playoff-odds-from-records.md` (586 words), step 2, the digest/retro sections that restate the prompt, prose length rules. | Port trimmed (~350): story-first summary, variance not luck, small-sample labels, close-game and all-play luck method. Drop the odds method unless a code tool computes odds. |
| history-grading | None. | Live. Contradiction 2. Efficiency formula and close-loss flag are tool output. | 701 words, plus 2,234. Cut: step 1's band declaration, the efficiency denominator (tool computes `fpts/ppts`), the "bad advice" list. | Port trimmed (~400): luck vs process split, aggregate don't scold, decision-time grading. |
| draft-strategy | None. | Dead with the draft job. | 689 words, plus 2,933. | Drop. |

### plugin.json, cards and workflows

`plugin.json` and the seven `agent-card.json` files are packaging for the v0 plugin loader. v1 agent definitions live in Postgres (A8) and extensions register their own schedules, so they do not port; `agent.yaml`'s tool lists become each definition's tool grants. The ten workflows are one node each (~55 words: agent, a task sentence, artifact kind). They carry no logic.

## Workflows: schedules (SCH-1) or agent prompts

v1 workflows are fixed steps in code with a model only where judgment is needed (D35), so each v0 one-node workflow becomes a v1 workflow whose first steps are today's tool-call instructions. What stays as prompt is only the analysis and writing step. Schedules (SCH-1) carry the trigger and the usefulness window (SCH-4, SC1). The times below are proposals for the Sleeper RFC owner to confirm, not measured.

| v0 workflow | v1 | Schedule and window | Prompt left |
| --- | --- | --- | --- |
| sleeper-lineup | Scheduled and on demand | Weekly, after the Wednesday report; deadline one hour before the first kickoff of the week; a second run before Sunday's kickoff, window to one hour before it. SL-9 sets the one-hour margin. | Decide slots from code-built facts (lock filter, best-by-projection baseline, injury and news). |
| sleeper-waivers | Scheduled and on demand | Before the league's waiver processing time (a `sleeper_league` setting); deadline one hour earlier. | Rank, size, name drops, from the code-built league-competition table. |
| sleeper-trade-finder | Scheduled and on demand | Weekly, after waivers settle, window until the next occurrence. | Choose and justify at most three deals from the code-built partner table. |
| sleeper-trade | On demand only | None. Input is an offer; trade talk keys on the trade. | Evaluate both rosters, write the verdict and the deciding line item. |
| sleeper-digest | Scheduled and on demand | Two occurrences: a preview before the week's first kickoff, a recap once the last game is final. Stale after the next occurrence. | Story, preview news. Code writes every number. |
| sleeper-retro | Scheduled and on demand | Once, after the week's last game, with the recap. | `why`, `lessons`, `summary`. Code builds the miss rows. |
| sleeper-trends | Scheduled and on demand | Daily, after the snapshot (S4), window until the next. | Dated one-sentence items and the source-conflict statement. |
| sleeper-history | On demand only | None. A season review has no deadline. | Narrative from code-built figures. |
| sleeper-season-notes | Drop | None. | Nothing reads the notes except the page; no evidence a note changed a decision. S4's league store (snapshots, suggestions, outcomes) is the cross-session state v1 asks for. |
| sleeper-draft | Drop | None. | None. |

Predictions (SL-1, SL-6) are not an agent prompt. They are Clef ranges produced and scored in code against outcomes, observe-only until they beat projections, and they get no rubric.

## Rubrics v1 needs

Every analysis card needs a rubric before it appears on the page (SL-8, D11). The v0 rubrics are 0-3 bands with 4-12 judge criteria each; the port follows the reviewer's form: atomic weighted checks, gates for what code can decide, penalties for named pitfalls. Two v0 findings shape it. Brevity and link provenance belong in schema caps and code checks, not the judge (#1722). A check that only verifies the agent's own bookkeeping verifies nothing (#1781). Weights below are starting values; D45 requires replay to show no check at its maximum on more than 90% of real rounds, and none of these has been replayed.

A check marked **code** is decided from the tool data the workflow already holds. Scopes: `card` is the whole artifact, `row` one slot, candidate, game or miss.

**Lineup card**

| Check | Weight | Resolver, scope | Question |
| --- | --- | --- | --- |
| every_slot_decided | gate | code | Does every roster slot, FLEX included, have a verdict? |
| no_locked_swap | gate | code | Does any recommended swap bench or bring in a player whose game is locked? |
| figures_match_tools | gate | code | Does each `proj` equal the tool's projection, and each lock time and opponent the player's own `game`? |
| summary_names_changes | gate | code | Does the summary name every difference from the current starters, or say there are none? |
| names_baseline_it_beats | 3 | model, row | Where a starter differs from the best-by-projection baseline, does `why` name the baseline player and what changed the call? |
| close_call_news | 3 | model, row | For a Questionable or limited-practice starter, or a bench player within the FLEX tie-break gap, does `why` cite news that bears on the call? (Link provenance is code.) |
| designation_weighted | 3 | model, row | Does the call treat an injury designation as a play rate, not as in or out? |
| picks_an_action | 2 | model, row | Does the row start, sit or keep, rather than leave it open? |
| matchup_overrides_talent | -4 | model, row | Does it bench a clearly better healthy player for matchup alone? |
| weather_mentioned_unmoved | -2 | model, card | Does it mention weather for a call weather did not move? |

**Waivers card**

| Check | Weight | Resolver, scope | Question |
| --- | --- | --- | --- |
| context_fields_match | gate | code | Do `waiver_type`, `my_priority` and `teams` equal the tool values? |
| drop_role_matches_roster | gate | code | Does each drop's role label match `starters`/bench, and is a starter drop stated as one? |
| widely_owned_accounted | gate | code | Is every free agent above the ownership or trending cut in `candidates` or `also_checked`? |
| sized_to_waiver_type | 4 | model, row | Is the claim sized in this league's own currency (share of remaining FAAB, or queue position)? |
| ranked_on_usage | 3 | model, row | Does the rank rest on role and usage, with trending counts framed as scarcity? |
| competition_read | 3 | model, card | Does urgency or size name a competing team's need, priority or FAAB left? |
| close_call_news | 3 | model, row | Does a close top claim or drop cite news? |
| bye_coverage | 2 | model, row | Does it flag a bye inside the lead time, and a shared bye as two slots? |
| crowd_metric_as_value | -3 | model, row | Is a trending or ownership number given as the reason the player is good? |
| ordered_fallback_flagged | -4 | model, card | Does it describe claims sharing a drop as a mistake? |
| faab_language_on_rolling | -4 | code, card | Does it use FAAB figures in a rolling or reverse-standings league? |

**Trade and trade-finder card**

| Check | Weight | Resolver, scope | Question |
| --- | --- | --- | --- |
| verdict_one_of | gate | code | Is the verdict exactly send, counter or decline (finder: at most three ranked)? |
| give_get_from_my_side | gate | code | Are `give` and `get` both from this team's side, with ids from the roster tools? |
| partner_modeled | 4 | model, card | Does the case rest on the partner's starting slots, record and surplus, not chart value alone? |
| deciding_line_item | 3 | model, card | Does it name the one line item that decides the call, with a condition that would flip it? |
| news_per_traded_player | 3 | model, row | Does each traded player have news cited? (Provenance is code.) |
| consolidation_priced | 2 | model, card | For a multi-for-one, are the discount and the forced cut priced? |
| chart_without_mismatch | -3 | model, card | Does it quote a 12-team chart without saying the league is smaller? |
| one_sided_as_collusion | -3 | model, card | Does it call a lopsided but voluntary deal collusion? |

**League report: digest card**

| Check | Weight | Resolver, scope | Question |
| --- | --- | --- | --- |
| every_game_covered | gate | code | Does the card hold every game in the week? |
| numbers_match_tools | gate | code | Does each score, margin and top scorer equal the tool value, and for a preview each side's points the code-computed `lineup_avg`? |
| no_projection_source | gate | code | Does the card contain a projection, projected total or win probability? |
| story_in_summary | 3 | model, card | Does the summary name the week's real story, not recite scores? |
| preview_news_decides | 3 | model, row | Does a preview game cite injury or role news that swings it? |
| small_sample_labeled | 2 | model, card | Are claims from under three weeks of data labeled as such? |
| luck_without_margin | -3 | model, card | Does it call a result lucky without the point margin, or from fewer than about eight close games? |
| game_flow_from_score | -3 | model, row | Does it describe a comeback or collapse that only the final score implies? |

**League report: retro card**

| Check | Weight | Resolver, scope | Question |
| --- | --- | --- | --- |
| tool_figures_copied | gate | code | Do `best`, `left`, `misses` and `waiver_misses` equal the tool's `best_points`, `left_on_bench`, `swaps` and `free_agent_hits`? |
| knowable_dated_before_kickoff | gate | code | Does each `knowable: true` row cite a signal dated before kickoff or the waiver run? |
| signal_is_evidence | 3 | model, row | Is a `knowable: true` signal a dated news item or trend, not the same designation restated? |
| hindsight_called_process | -4 | model, row | Is a `knowable: false` outcome blamed on the manager? |
| lessons_actionable | 2 | model, card | Are the lessons (at most three) things to do next week? |
| summary_names_swing | 2 | model, card | Does the summary name the single biggest swing? |

**History card**

| Check | Weight | Resolver, scope | Question |
| --- | --- | --- | --- |
| figures_from_tools | gate | code | Does each record, efficiency, PF/PA and verdict equal the tool value, under the one declared reach/steal definition? |
| luck_process_split | 3 | model, card | Does it separate lineup execution and draft from schedule and player-outcome luck? |
| aggregated_not_scolded | 3 | model, row | Is a criticism backed by a pattern across weeks, naming player, pick or week, and number? |
| decision_time_grading | 2 | model, row | Is a pre-kickoff call judged on what was knowable then? |
| single_week_as_pattern | -3 | model, row | Is one week, bust or trade offered as evidence of a bad manager? |
| efficiency_as_record | -2 | model, card | Does it equate lineup efficiency with wins or losses? |

**Trends timeline**

| Check | Weight | Resolver, scope | Question |
| --- | --- | --- | --- |
| item_shape | gate | code | Does each item carry time, source, kind and one sentence within the schema caps? |
| link_fetched | gate | code | Was each linked page fetched or seen in a search this session? |
| conflict_stated | 3 | model, row | Where Sleeper and the web disagree, does the item say so and name the rule followed? (Needs contradiction 3 settled first.) |
| original_source_named | 2 | model, row | Does the source name the reporter and outlet, not an aggregator? |
| aggregator_only | -3 | model, row | Is an aggregator the only source? |

Predictions have no rubric; the scorer (S4, SL-6) compares them with what happened.

## Follow-ups

- Settle contradictions 1-5 in the port PR for each skill, not as a separate sweep, so each fix lands with the item it changes. Contradiction 3 (who wins on a conflict) is a decision for the owner, not a wording fix.
- Decide whether v1 wants playoff odds at all. If so, they are a code tool (a simulation, which the v0 reference itself says real products use) and the digest narrates the output. The closed-form text does not port.
- Replay the ported rubrics before they gate anything (D45). The lineup and waiver weights are guesses until that run.
- Waiver-scout and lineup-analyst are the first ports worth an A/B: they are the largest prompts and the only ones with a running weekly use. #1726 shows a trimmed prompt can hold quality, but only for trend-scout.
