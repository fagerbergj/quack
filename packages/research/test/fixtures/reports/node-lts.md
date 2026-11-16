## Summary
Node.js 26 (codename "Lithium") is the current Active LTS release line; per the official release schedule, it moves to Maintenance LTS on October 20, 2027 (with end-of-life on April 30, 2029).

## Assumptions
- "Current Active LTS" is taken as the most recently released line in Active LTS status, as of late October 2026. Node.js's releases page lists supported lines by "LTS" status without an explicit Active/Maintenance column, so the Active vs. Maintenance distinction is drawn from the Release Working Group's `schedule.json` dates. [s1][s2]
- The exact "as of" date of the sources is around October 28–29, 2026 (latest entries in the fetched pages). [s1][s2]

## Findings
- The Node.js Release Working Group schedule lists v26 (codename "Lithium") with first release 2026-05-05, LTS start 2026-10-28, maintenance start 2027-10-20, and end-of-life 2029-04-30. [s1]
- The schedule likewise shows v24 ("Krypton") entering maintenance on 2026-10-20 (LTS since 2025-10-28, EOL 2028-04-30) and v22 ("Jod") in maintenance since 2025-10-21, so v26 is the only even-numbered line currently in its Active LTS window. [s1]
- The Node.js website's releases page lists v26 "Lithium" (first released May 05, 2026, last updated Oct 28, 2026) with "LTS" status, and labels v26.13.0 as the "Latest LTS"; v24 "Krypton" and v22 "Jod" are also shown as LTS. [s2]
- Node.js's release model: even-numbered releases up through Node.js 26 move to Active LTS after a six-month Current phase, and LTS status typically guarantees fixes for a total of 30 months; starting with Node.js 27 the cycle becomes annual with every major version moving to LTS after six months Current. [s2]
- The releases page footer identifies "v26.13.0" as the "Latest LTS" version, consistent with v26 being the newest line in LTS (Active) status. [s2]
