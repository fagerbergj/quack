// Labelled research reports for replaying the model checks: good reports and reports seeded with one flaw each.
// The entities are invented so the judge's own knowledge cannot settle a case; each source text is a short page.
import { inputsOf } from '@quack/research';

export type Target = 'supported_by_source' | 'contradicted_by_source' | 'answers_every_ask' | 'summary_follows_findings' | 'no_self_contradiction';
export type Labelled = { case: string; kind: 'research.report'; content: string; inputs: ReturnType<typeof inputsOf>; good: boolean; targets: Target[] };

const REQUEST = {
  harbour: "What budget did Port Alder's council approve for harbour repairs in 2026, and when does the work start?",
  buses: 'How many electric buses does Kestrel Transit run as of 2026, and what is their rated range?',
  busCost: 'How many electric buses does Kestrel Transit run as of 2026, what is their rated range, and how much did each cost?',
  library: 'When is Marlow Public Library open on weekends in 2026, and is parking free?',
  grant: 'Who is eligible for the Fernhill Small Farm Grant in 2026, and what is the maximum award?',
};

const SOURCE = {
  harbour: 'Port Alder Council Minutes, 14 January 2026\n\nItem 6: Harbour repairs. The council voted 6 to 1 to approve a budget of $4.2 million for repairs to the east breakwater and the ferry pier. Engineering staff said the contractor, Tidewater Marine, will mobilise on 2 March 2026 and expects to finish by October 2026. The budget is drawn from the harbour reserve fund; no new borrowing is required. Councillor Okafor dissented, citing the cost of the pier deck replacement.\n\nItem 7: Library roof. The council received a report on the Elm Street library roof and deferred a decision to the February meeting.',
  harbour2025: 'Port Alder Council Minutes, 14 January 2025\n\nItem 6: Harbour repairs. The council voted 5 to 2 to approve a budget of $3.1 million for repairs to the east breakwater. Engineering staff said the contractor, Tidewater Marine, will mobilise on 3 March 2025 and expects to finish by September 2025. The budget is drawn from the harbour reserve fund.',
  buses: 'Kestrel Transit fleet report, March 2026\n\nKestrel Transit operates 38 battery-electric buses, up from 24 in 2025, out of a fleet of 112. All are the Vantor E12 model, rated at 190 km on a full charge in mixed city driving. Charging is at the Northside and Quay depots. The agency plans to add 12 more electric buses in 2027.',
  suppliers: 'Kestrel Transit procurement note, 2026\n\nKestrel Transit buys its electric buses from Vantor. Kestrel Transit buys the depot chargers for its electric buses from Voltra.',
  library: 'Marlow Public Library: hours effective 5 January 2026\n\nMonday to Friday 9am to 8pm. Saturday 10am to 5pm. Sunday 1pm to 5pm. Closed on public holidays. Visitor parking: the Elm Street lot is free for the first three hours, then $2 per hour; the library validates tickets for borrowers.',
  library2025: 'Marlow Public Library: hours effective 6 January 2025\n\nMonday to Friday 9am to 8pm. Saturday 10am to 5pm. Sunday 1pm to 5pm. Closed on public holidays. Visitor parking: the Elm Street lot is free for the first three hours, then $2 per hour.',
  grant: 'Fernhill Small Farm Grant 2026 guidelines\n\nEligible applicants are farms of under 40 hectares in Fernhill County that earned less than $250,000 in gross sales in the prior year. Applicants must have operated for at least two years. The maximum award is $15,000 per farm; awards cover equipment and infrastructure only, not wages. Applications open 1 April 2026 and close 31 May 2026.',
};

const report = (summary: string, findings: string[], assumptions = 'None.') => `## Summary\n${summary}\n\n## Assumptions\n${assumptions}\n\n## Findings\n${findings.map((f) => `- ${f} [s1]`).join('\n')}\n`;
const SUMMARY_LINE = /^## Summary\n.*$/m;

const GOOD = {
  harbour: report("Port Alder's council approved $4.2 million for harbour repairs in January 2026, and work starts on 2 March 2026.", ['The council approved $4.2 million for repairs to the east breakwater and ferry pier on 14 January 2026.', 'The contractor, Tidewater Marine, mobilises on 2 March 2026 and expects to finish by October 2026.', 'The money comes from the harbour reserve fund, with no new borrowing.']),
  buses: report('Kestrel Transit runs 38 electric buses in 2026, each rated at 190 km per charge.', ['Kestrel Transit operates 38 battery-electric buses out of a fleet of 112, up from 24 in 2025.', 'All are the Vantor E12, rated at 190 km on a full charge in mixed city driving.']),
  library: report('Marlow Public Library is open Saturday 10am to 5pm and Sunday 1pm to 5pm in 2026; parking is free for three hours.', ['Hours effective 5 January 2026: Saturday 10am to 5pm and Sunday 1pm to 5pm.', 'The Elm Street lot is free for the first three hours, then $2 per hour, and the library validates tickets for borrowers.']),
  grant: report('Farms under 40 hectares in Fernhill County with under $250,000 in sales can apply for up to $15,000.', ['Eligible farms are under 40 hectares, in Fernhill County, with gross sales under $250,000 in the prior year and at least two years of operation.', 'The maximum award is $15,000 per farm, for equipment and infrastructure only, not wages.', 'Applications open 1 April 2026 and close 31 May 2026.']),
};

type Spec = { name: string; request: string; source: string; content: string; targets?: Target[] };

const SPECS: Spec[] = [
  { name: 'good-harbour', request: REQUEST.harbour, source: SOURCE.harbour, content: GOOD.harbour },
  { name: 'good-buses', request: REQUEST.buses, source: SOURCE.buses, content: GOOD.buses },
  { name: 'good-library', request: REQUEST.library, source: SOURCE.library, content: GOOD.library },
  { name: 'good-grant', request: REQUEST.grant, source: SOURCE.grant, content: GOOD.grant },
  // Two companies in two roles: no_self_contradiction must not call them a conflict.
  { name: 'good-two-suppliers', request: 'Who supplies Kestrel Transit\'s electric buses and their chargers?', source: SOURCE.suppliers, content: report('Vantor builds Kestrel Transit\'s electric buses and Voltra supplies the chargers.', ['Kestrel Transit buys its electric buses from Vantor.', 'Kestrel Transit buys the depot chargers for its electric buses from Voltra.']) },
  { name: 'good-unsettled-cost', request: REQUEST.busCost, source: SOURCE.buses, content: report('Kestrel Transit runs 38 electric buses rated at 190 km; the sources give no per-bus cost.', ['Kestrel Transit operates 38 battery-electric buses, all Vantor E12 rated at 190 km on a full charge.'], 'The fleet report states no purchase price, so the cost per bus is unsettled.') },

  { name: 'unsupported-harbour', request: REQUEST.harbour, source: SOURCE.harbour, targets: ['supported_by_source'], content: GOOD.harbour.replace('The money comes from the harbour reserve fund, with no new borrowing.', 'The repairs include dredging the main shipping channel to 9 metres.') },
  { name: 'unsupported-library', request: REQUEST.library, source: SOURCE.library, targets: ['supported_by_source'], content: GOOD.library.replace('The Elm Street lot is free for the first three hours, then $2 per hour, and the library validates tickets for borrowers.', 'Weekend story time for children runs on Saturdays at 11am.') },
  { name: 'unsupported-grant', request: REQUEST.grant, source: SOURCE.grant, targets: ['supported_by_source'], content: GOOD.grant.replace('Applications open 1 April 2026 and close 31 May 2026.', 'Applicants must hold organic certification.') },

  { name: 'contradicted-harbour', request: REQUEST.harbour, source: SOURCE.harbour, targets: ['contradicted_by_source'], content: GOOD.harbour.replaceAll('2 March 2026', '2 June 2026') },
  { name: 'contradicted-buses', request: REQUEST.buses, source: SOURCE.buses, targets: ['contradicted_by_source'], content: GOOD.buses.replaceAll('190 km', '240 km') },
  { name: 'contradicted-grant', request: REQUEST.grant, source: SOURCE.grant, targets: ['contradicted_by_source'], content: GOOD.grant.replaceAll('$15,000', '$25,000') },

  { name: 'unanswered-harbour-start', request: REQUEST.harbour, source: SOURCE.harbour, targets: ['answers_every_ask'], content: report("Port Alder's council approved $4.2 million for harbour repairs in January 2026.", ['The council approved $4.2 million for repairs to the east breakwater and ferry pier on 14 January 2026.', 'The money comes from the harbour reserve fund, with no new borrowing.']) },
  { name: 'unanswered-buses-cost', request: REQUEST.busCost, source: SOURCE.buses, targets: ['answers_every_ask'], content: GOOD.buses },
  { name: 'unanswered-library-parking', request: REQUEST.library, source: SOURCE.library, targets: ['answers_every_ask'], content: report('Marlow Public Library is open Saturday 10am to 5pm and Sunday 1pm to 5pm in 2026.', ['Hours effective 5 January 2026: Saturday 10am to 5pm and Sunday 1pm to 5pm, closed on public holidays.']) },

  { name: 'wrong-year-harbour', request: REQUEST.harbour, source: SOURCE.harbour2025, targets: ['supported_by_source'], content: report("Port Alder's council approved $3.1 million for harbour repairs in January 2026, and work starts on 3 March 2026.", ['The council approved $3.1 million for repairs to the east breakwater on 14 January 2026.', 'The contractor, Tidewater Marine, mobilises on 3 March 2026 and expects to finish by September 2026.']) },
  { name: 'wrong-year-library', request: REQUEST.library, source: SOURCE.library2025, targets: ['answers_every_ask'], content: report('Marlow Public Library is open Saturday 10am to 5pm and Sunday 1pm to 5pm; parking is free for three hours.', ['Hours effective 6 January 2025: Saturday 10am to 5pm and Sunday 1pm to 5pm.', 'The Elm Street lot is free for the first three hours, then $2 per hour.']) },

  { name: 'summary-overreach-grant', request: REQUEST.grant, source: SOURCE.grant, targets: ['summary_follows_findings'], content: GOOD.grant.replace(SUMMARY_LINE, '## Summary\nFarms under 40 hectares in Fernhill County can get up to $15,000, and decisions are made within 30 days.') },
  { name: 'summary-overreach-harbour', request: REQUEST.harbour, source: SOURCE.harbour, targets: ['summary_follows_findings'], content: GOOD.harbour.replace(SUMMARY_LINE, "## Summary\nPort Alder's council approved $4.2 million for harbour repairs, work starts on 2 March 2026, and the repairs will halve ferry delays.") },

  { name: 'self-contradiction-buses', request: REQUEST.buses, source: SOURCE.buses, targets: ['no_self_contradiction'], content: GOOD.buses.replace('All are the Vantor E12', 'All 28 electric buses are the Vantor E12') },
  { name: 'self-contradiction-harbour-name', request: REQUEST.harbour, source: SOURCE.harbour, targets: ['no_self_contradiction'], content: GOOD.harbour.replace(SUMMARY_LINE, "## Summary\nPort Alder's council approved $4.2 million for harbour repairs in January 2026. The contractor, Harbour Works, mobilises on 2 March 2026.") },
  { name: 'self-contradiction-library', request: REQUEST.library, source: SOURCE.library, targets: ['no_self_contradiction'], content: GOOD.library.replace('Saturday 10am to 5pm and Sunday 1pm to 5pm.', 'Saturday 9am to 5pm and Sunday 1pm to 5pm.') },
];

/** The cases with inputs built by the code the live judge uses; every cited source is `s1`. */
export function researchCases(): Labelled[] {
  return SPECS.map((s) => ({
    case: s.name, kind: 'research.report', content: s.content, good: s.name.startsWith('good-'), targets: s.targets ?? [],
    inputs: inputsOf(s.content, s.request, new Map([['s1', s.source]]), ['s1']),
  }));
}
