// Labelled research reports for replaying the model checks: clean reports and reports seeded with one style flaw each.
// Every case links only its one fetched source, so the link gate passes and only a style check can catch its flaw.
import { inputsOf } from '@quack/research';

export type Target = 'no_purpose' | 'prose_list' | 'topic_sentence' | 'wordy_phrase' | 'list_not_parallel';
export type Labelled = { case: string; kind: 'research.report'; content: string; inputs: ReturnType<typeof inputsOf>; good: boolean; targets: Target[] };

const report = (summary: string, findings: string[], assumptions = 'None.') => `## Summary\n${summary}\n\n## Assumptions\n${assumptions}\n\n## Findings\n${findings.map((f) => `- ${f} [s1]`).join('\n')}\n`;

const GOOD = {
  harbour: report("Port Alder's council approved $4.2 million for harbour repairs in January 2026, and work starts on 2 March 2026.", ['The council approved $4.2 million for repairs to the east breakwater and ferry pier on 14 January 2026.', 'The contractor, Tidewater Marine, mobilises on 2 March 2026 and expects to finish by October 2026.', 'The money comes from the harbour reserve fund, with no new borrowing.']),
  buses: report('Kestrel Transit runs 38 electric buses in 2026, each rated at 190 km per charge.', ['Kestrel Transit operates 38 battery-electric buses out of a fleet of 112, up from 24 in 2025.', 'All are the Vantor E12, rated at 190 km on a full charge in mixed city driving.', 'The buses charge at the Northside and Quay depots.']),
  library: report('Marlow Public Library is open Saturday 10am to 5pm and Sunday 1pm to 5pm in 2026; parking is free for three hours.', ['Hours effective 5 January 2026: Saturday 10am to 5pm and Sunday 1pm to 5pm.', 'The Elm Street lot is free for the first three hours, then $2 per hour.', 'The library validates parking tickets for borrowers.']),
  grant: report('Farms under 40 hectares in Fernhill County with under $250,000 in sales can apply for up to $15,000.', ['Eligible farms are under 40 hectares, in Fernhill County, with gross sales under $250,000 in the prior year.', 'The maximum award is $15,000 per farm, for equipment and infrastructure only.', 'Applications open 1 April 2026 and close 31 May 2026.'], 'The question asks about 2026, so this report reads the 2026 guidelines. It does not cover earlier rounds.'),
};

const SPECS: Array<{ name: string; content: string; targets?: Target[] }> = [
  { name: 'good-harbour', content: GOOD.harbour },
  { name: 'good-buses', content: GOOD.buses },
  { name: 'good-library', content: GOOD.library },
  { name: 'good-grant', content: GOOD.grant },
  { name: 'good-labelled-list', content: report('Kestrel Transit runs 38 electric buses in 2026.', ['**Fleet:** 38 battery-electric buses out of 112.', '**Model:** the Vantor E12, rated at 190 km.']) },

  { name: 'no-purpose-harbour', targets: ['no_purpose'], content: GOOD.harbour.replace(/^## Summary\n.*$/m, '## Summary\nThe notes below were gathered over a week of reading.') },
  { name: 'no-purpose-buses', targets: ['no_purpose'], content: GOOD.buses.replace(/^## Summary\n.*$/m, '## Summary\nMany things are worth saying about transit.') },
  { name: 'prose-list-harbour', targets: ['prose_list'], content: GOOD.harbour.replace('## Assumptions\nNone.', '## Assumptions\nThe minutes cover the breakwater repairs, the ferry pier works, the pier deck replacement, the reserve fund draw and the library roof report, so this report reads only the first four.') },
  { name: 'prose-list-library', targets: ['prose_list'], content: GOOD.library.replace('## Assumptions\nNone.', '## Assumptions\nTo check the hours, first open the library site, then find the hours page, then pick the 2026 notice, then compare it with the 2025 notice, then read the parking section.') },
  { name: 'topic-sentence-grant', targets: ['topic_sentence'], content: GOOD.grant.replace('The question asks about 2026, so this report reads the 2026 guidelines. It does not cover earlier rounds.', 'Fernhill County is known for its apple orchards. The question asks about the 2026 grant round, so this report reads the 2026 guidelines. Earlier rounds had other limits and are not covered here.') },
  { name: 'topic-sentence-buses', targets: ['topic_sentence'], content: GOOD.buses.replace('## Assumptions\nNone.', '## Assumptions\nKestrel is a small bird of prey. This report counts only battery-electric buses, not hybrids. The fleet report from March 2026 is the latest one available.') },
  { name: 'wordy-buses', targets: ['wordy_phrase'], content: GOOD.buses.replace('The buses charge at the Northside and Quay depots.', 'In order to charge, the buses use the Northside and Quay depots.') },
  { name: 'wordy-harbour', targets: ['wordy_phrase'], content: GOOD.harbour.replace('The money comes from the harbour reserve fund, with no new borrowing.', 'Due to the fact that the money comes from the harbour reserve fund, no new borrowing is required.') },
  { name: 'mixed-list-library', targets: ['list_not_parallel'], content: GOOD.library.replace('- The Elm Street lot', '- **Parking:** the Elm Street lot').replace('- The library validates parking tickets for borrowers.', '- Validate your parking ticket at the desk.') },
  { name: 'mixed-list-grant', targets: ['list_not_parallel'], content: GOOD.grant.replace('- The maximum award', '- **Award:** the maximum award').replace('- Applications open 1 April 2026 and close 31 May 2026.', '- Apply between 1 April and 31 May 2026.') },
];

/** The cases with inputs built by the code the live judge uses; each cites `s1`, fetched from its own page. */
export function researchCases(): Labelled[] {
  return SPECS.map((s) => ({
    case: s.name, kind: 'research.report', content: s.content, good: s.name.startsWith('good-'), targets: s.targets ?? [],
    inputs: inputsOf(s.content, [{ id: 's1', url: `https://${s.name}.test/source`, fetchedAt: 0, hash: '' }], []),
  }));
}
