# Executable Work Items

An executable issue is a bounded description of work, not a complete implementation design. It gives an assignee enough context to start and enough verification detail to finish without freezing every design choice in advance.

## Choose the work-item shape

### Behavior or feature

Use when completion changes what a user or consuming system can do:

- problem, user, and value;
- desired outcome;
- scope and non-goals;
- acceptance criteria;
- relevant constraints, designs, or decisions.

A user-story sentence can clarify role, goal, and benefit, but do not force it when a direct problem statement is clearer. Linear explicitly prefers concrete issues over ritualized user-story wording; Atlassian, Asana, and GitLab document contexts where user stories remain useful.

Sources: [Linear: Write Issues, Not User Stories](https://linear.app/method/write-issues-not-user-stories), [Atlassian user stories](https://www.atlassian.com/agile/project-management/user-stories), [GitLab good user stories](https://handbook.gitlab.com/handbook/customer-success/professional-services-engineering/professional-services-delivery-methodology/good-user-stories/)

### Bug or regression

Use when current observable behavior differs from expected behavior:

- impact and affected surface;
- expected and actual behavior;
- minimal reproduction and environment;
- acceptance criteria, including regression protection where appropriate.

Do not require the reporter to know the root cause or implementation.

### Engineering or documentation task

Use when the outcome is concrete but not naturally a user story:

- reason the work matters;
- exact deliverable or behavior change;
- boundaries and constraints;
- verification conditions;
- links to the decision, design, or parent outcome.

Connect maintenance work to risk, support burden, compatibility, cost, or delivery capability. Do not invent a fictional persona.

### Investigation or spike

Use when uncertainty prevents a responsible implementation issue:

- question to answer;
- why the answer is needed;
- evidence or experiments to gather;
- time or effort boundary if the team uses one;
- expected output, such as a recommendation, measurement, ADR, RFC, or follow-up issues.

A spike completes when it reduces the named uncertainty, not when it accidentally ships a production feature.

### Parent item or epic

Use when one outcome requires multiple independently verifiable issues. Keep the parent focused on shared context, outcome, sequencing, and child links. Do not duplicate every child’s acceptance criteria in the parent.

## Readiness heuristics

INVEST is a useful review mnemonic, not a universal gate:

- **Independent:** minimize avoidable coupling and name unavoidable dependencies.
- **Negotiable:** preserve room to choose an implementation unless a decision already constrains it.
- **Valuable:** connect work to an observable benefit, risk reduction, or capability.
- **Estimable:** expose enough uncertainty for the team to size or split the work.
- **Small:** fit the team’s normal delivery loop.
- **Testable:** define observable completion.

A team-specific Definition of Ready may additionally require designs, API contracts, estimates, dependency resolution, or team review. Do not impose one organization’s checklist on another. Scrum does not prescribe a Definition of Ready.

Sources: [Martin Fowler on user stories and INVEST](https://martinfowler.com/bliki/UserStory.html), [Roman Pichler on Definition of Ready](https://www.romanpichler.com/blog/the-definition-of-ready/), [Scrum.org on analysis paralysis](https://www.scrum.org/resources/blog/definition-ready-analysis-paralysis-waiting-perfect-information)

## Split work around value and verification

Split an issue when it contains unrelated outcomes, cannot be understood or estimated, has acceptance criteria for several releases, or requires long idle handoffs.

Prefer vertical slices that produce independently verifiable behavior. Avoid horizontal tickets such as database, API, UI, testing, review, and deployment when none provides a complete outcome alone. Exact batch-size targets are local: use the team’s delivery cadence rather than presenting two days, one week, or one sprint as a universal rule.

## Bounded default shape

When the tracker has no template, use:

1. **Problem:** observable gap between current and desired behavior.
2. **Impact:** affected user or system and why the work matters.
3. **Outcome:** behavior or artifact that will exist afterward.
4. **Scope and non-goals:** boundaries, constraints, and exclusions.
5. **Acceptance criteria:** concrete conditions that prove completion.
6. **References:** decisions, designs, evidence, dependencies, and related work.

Add test cases when they communicate important positive, failure, or compatibility behavior. Do not duplicate the project’s general Definition of Done in every issue.

## Common failure modes

- solution-first titles such as `Add queue` before the behavior gap is understood;
- acceptance criteria that restate implementation tasks rather than observable results;
- several outcomes joined by “and”;
- hidden dependency or decision work;
- over-specified tickets that prevent a better implementation;
- empty template sections and placeholder text;
- estimates or priority assigned without the responsible team.
