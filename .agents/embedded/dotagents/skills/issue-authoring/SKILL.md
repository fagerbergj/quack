---
name: issue-authoring
description: >
  Authors and reviews executable issues, tickets, work items, bug reports, and feature requests
  across GitHub, Jira, Linear, Azure DevOps, and similar trackers. Use when work needs a clear
  outcome, bounded scope, sufficient context, and testable completion criteria before assignment.
license: MIT
metadata:
  author: fagerbergj
  author_url: https://github.com/fagerbergj
  repository: https://github.com/fagerbergj/dotagents
  version: "1.0.1"
---

# Issue Authoring

## Overview

Write the smallest durable issue that lets the next person understand why the work matters, what outcome is in scope, and how completion will be verified. Follow the project’s tracker conventions and templates first. This skill improves the work description; it does not choose the tracker, assign priority, estimate for the team, or create an external issue unless the user asks.

## When to Use

Use this skill to create or revise a bug report, feature request, engineering task, user story, spike, backlog item, or other bounded unit of tracked work. It is especially useful before assignment when missing context would otherwise cause another conversation.

## When NOT to Use

- Use an RFD when the problem or solution space still needs exploratory discussion.
- Use an RFC when a substantial proposal needs formal review before becoming executable work.
- Use an ADR to preserve why a decision was made.
- Use a pull request description to explain an implemented change.
- Do not create a ticket for a question, reminder, or trivial action that the team does not need to track.

## Authoring Procedure

1. **Find the house standard.** Inspect contribution docs, issue templates or forms, existing well-maintained issues, required fields, title prefixes, labels, workflow states, and definitions of ready or done. Local rules override this skill.
2. **Classify the work.** Identify whether this is a bug report, feature or behavior request, bounded implementation task, documentation change, investigation, or parent item. Load the matching reference before drafting.
3. **Search before creating.** Check for duplicates and related work. Add evidence to an existing issue when it represents the same outcome; link related but distinct issues.
4. **Define one outcome.** State the observable gap or requested result, who or what is affected, and why it matters. Split unrelated outcomes and parent-sized work.
5. **Bound the contract.** Record relevant constraints, non-goals, dependencies, and known evidence. Separate facts from hypotheses. Do not prescribe implementation unless it is an actual constraint or accepted decision.
6. **Make completion testable.** Write concrete acceptance criteria or verification conditions. For investigations, define the question, timebox if the team uses one, and expected artifact or decision.
7. **Apply tracker metadata.** Use only labels, priority, estimates, owners, milestones, and dependencies supported by the house workflow. Never invent team estimates or priority.
8. **Run the validation loop.** Revise until a fresh implementer can start without guessing about scope or completion. Put or create the issue wherever the user or session requires.

## Default Shape

A section is filled from the report or it is left out. Never supply a product area, endpoint, error code, version, severity, customer, or reproduction step the report did not contain in order to complete a section - a plausible invention is worse than a gap, because the reader cannot tell it from a fact. If the report does not identify a product, a behaviour, or a failure, the answer is not an issue at all: say the report is insufficient and ask the specific questions that would unblock it.

When no house template exists, keep the issue compact:

- **Title:** specific outcome or observed failure, including the affected area.
- **Context:** present behavior, affected user or system, and why the work matters now.
- **Outcome:** behavior that should be true when complete.
- **Scope and non-goals:** boundaries, constraints, and exclusions that prevent scope creep.
- **Acceptance criteria:** observable pass/fail conditions.
- **Evidence and references:** reproduction, logs, screenshots, designs, decisions, related issues, or source links as needed.

Omit empty sections - omitting is the correct outcome when the report supports nothing, not a shortfall to be papered over. A short, complete issue is better than a large form filled with placeholders.

## Gotchas

- “As a … I want … so that …” is useful when role, goal, and value are genuinely unclear; it is not mandatory. Direct problem statements are often better for engineering work.
- INVEST and Definition of Ready are review heuristics, not universal laws. Teams decide readiness, estimation, and batch-size targets.
- Small means small enough for the project’s delivery loop, not an arbitrary global hour limit. Split vertically around independently verifiable behavior rather than into database, API, UI, test, and review phases.
- Acceptance criteria describe observable completion, not a checklist of implementation guesses.
- Screenshots support visual problems. Logs, stack traces, commands, and configuration belong in searchable text blocks with secrets removed.

## Validation Loop

Before sharing or creating the issue, verify:

- the title distinguishes this issue in list and search views;
- one outcome is in scope and exclusions are explicit where ambiguity is likely;
- facts, assumptions, and proposed solutions are distinguishable;
- acceptance criteria prove the outcome without forcing an unnecessary implementation;
- reproduction and environment details are sufficient for reported defects;
- dependencies, references, and security-sensitive data are handled correctly;
- the issue follows the house template, workflow, and metadata rules.

If the work cannot be estimated or started because a consequential decision is missing, do not pad the issue. Ask for the decision, create an investigation, or move the topic to the appropriate discussion artifact.

## Resources

- Read `references/github-issues.md` when authoring public or private GitHub issues, bug reports, feature requests, issue forms, titles, or labels.
- Read `references/executable-work.md` when authoring implementation-ready tickets, user stories, spikes, backlog items, or work that may need splitting.
