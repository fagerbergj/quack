# Schedules extension architecture

- **Status:** Accepted, 2026-10-30
- **Requirements:** [schedules](../../extensions/schedules.md) · **Core:** [RFC 0001](../0001-core.md)

## Summary

Schedules is a service half with a durable store of schedules and a timer loop that submits to session keys.

## Design

- **Registration.** Other service halves (Sleeper) and agent definitions register schedules. Each has a cron expression or explicit times, a session key template, the input to submit, and a usefulness window: a deadline or a staleness period, by default the next occurrence (SCH-1, SC1).
- **Firing.** Each occurrence submits with the request id `schedule:<id>:<occurrence time>`, so a retry or duplicate firing is the same submission (HAR-5).
- **Downtime.** At start the loop finds missed occurrences. It submits the newest one still inside its window once, merging the others into it, and records the rest as missed (SCH-4).
- **Visibility.** The operate view lists schedules with their last and next runs and any missed ones (SCH-3).

## Unresolved questions

None beyond the core RFC.
