# GitHub Issue Authoring

Use the repository’s contribution guide and issue form first. GitHub projects differ: Rails directs open-ended feature discussion elsewhere, Kubernetes uses formal SIG triage, and smaller projects may accept a short free-form issue.

## Before opening

1. Search open and closed issues using the component, error text, and likely synonyms.
2. Confirm the repository uses Issues for this request. Support questions, security reports, and feature discussions may have separate channels.
3. Remove secrets, private data, tokens, and customer identifiers from every attachment and log.
4. Select the repository’s bug, feature, documentation, or other issue form. Do not bypass required fields with placeholders.

OpenTelemetry’s contributor guide is a useful public baseline: [Writing a Good GitHub Issue](https://github.com/open-telemetry/community/blob/main/guides/contributor/good-github-issues.md).

## Titles

Make the title searchable and specific about the affected area and behavior.

Prefer:

- `HTTP retries ignore the configured backoff interval`
- `Safari 17: disable Login button while authentication is pending`
- `CLI docs link to the removed config reference`

Avoid reactions or generic requests such as `It is broken`, `Docs are wrong`, or `Please fix login`. Follow component prefixes or title formats already used by the project.

Sources: [OpenTelemetry guide](https://github.com/open-telemetry/community/blob/main/guides/contributor/good-github-issues.md), [GitHub issue-writing guidance](https://github.blog/developer-skills/github/how-to-create-issues-and-pull-requests-in-record-time-on-github/)

## Bug report shape

Use the house form. Otherwise include only relevant fields:

- concise problem and impact;
- exact expected behavior;
- exact actual behavior;
- minimal numbered reproduction steps;
- smallest runnable example or repository when needed;
- package, SDK, runtime, browser, and operating-system versions;
- relevant configuration;
- searchable logs or stack traces in fenced text blocks;
- regression range or last known working version, if known.

Write steps for someone who has never seen the setup. Literal values matter: `Type "Test"` is reproducible; `type some text` may hide a case-sensitive failure.

Rails demonstrates a stronger option for framework bugs: executable report templates that produce a minimal failing test case. Spring similarly asks reporters to reduce problems from a vanilla generated application. pandas triage treats missing reproducibility as missing information rather than proof that no bug exists.

Sources: [Rails bug reports](https://guides.rubyonrails.org/contributing_to_ruby_on_rails.html#creating-a-bug-report), [Spring help guide](https://github.com/spring-projects/spring-ws/wiki/How-To-Get-Help), [pandas maintenance guide](https://pandas.pydata.org/docs/development/maintaining.html), [Crafting Minimal Bug Reports](https://matthewrocklin.com/blog/2018/02/28/minimal-bug-reports)

## Feature request shape

Start with the problem and concrete use case, not the desired implementation:

- affected user or workflow;
- present limitation and impact;
- desired outcome;
- non-goals or compatibility constraints;
- optional solution and alternatives, clearly labeled as proposals;
- acceptance criteria when the request is ready for implementation.

Open-ended design debate may belong in a discussion or RFC rather than an issue. Rails explicitly routes involved feature requests to its discussion forum; follow the project’s equivalent rule.

## Evidence and attachments

- Paste logs, traces, commands, and configuration as text so they remain searchable and copyable.
- Use screenshots or recordings for visual state, layout, animation, or interactions that text cannot preserve.
- Include the smallest evidence that demonstrates the issue. Large logs should identify the relevant interval and failure.
- Never speculate about root cause as if it were observed fact.

## Labels and metadata

Contributors usually should not guess labels when forms or maintainers own triage. Issue forms can apply initial labels automatically. Mature projects may encode type, component, owner, priority, milestone, and contributor suitability, but each taxonomy is local.

`good first issue` is a maintainer commitment that the work is understood, bounded, and supported. Do not apply it merely because a change looks small.

Sources: [GitHub issue templates](https://docs.github.com/en/communities/using-templates-to-encourage-useful-issues-and-pull-requests/about-issue-and-pull-request-templates), [Kubernetes issue triage](https://www.kubernetes.dev/docs/guide/issue-triage/), [pandas maintenance guide](https://pandas.pydata.org/docs/development/maintaining.html)

## Issue forms are repository design

When asked to design intake rather than write one issue, prefer the fewest forms that match real traffic. GitHub supports Markdown templates and YAML issue forms with required fields, dropdowns, uploads, validation, and automatic labels. Do not disable blank issues unless the project has clear alternative routes for support, security, and requests that do not fit a form.
