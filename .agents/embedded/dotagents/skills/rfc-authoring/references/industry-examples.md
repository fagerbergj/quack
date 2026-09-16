# RFC industry examples

Use these examples to select a shape, not to assemble a maximal template. A repository’s own format and approval rules take precedence.

## Document shapes

### Mozilla Firefox Android RFC

**Fits:** A substantial product or platform change that needs an accessible explanation plus technical detail.

**Published shape:**

- Summary
- Motivation
- Guide-level explanation
- Reference-level explanation (optional)
- Drawbacks (optional)
- Rationale and alternatives
- Resources and Docs (optional)
- Unresolved questions

**Process:** Early feedback precedes the RFC. Android technical stewards accept or reject it after the feedback phase; stakeholders provide input but are not all necessarily approvers.

Sources: [Firefox Android RFC process](https://firefox-source-docs.mozilla.org/mobile/android/rfcs/0015-android-rfc-process.html), [Firefox Android RFC template](https://github.com/mozilla-firefox/firefox/blob/main/mobile/android/docs/rfcs/0000-template.md)

### Rust RFC

**Fits:** A substantial language, library, Cargo, or project-level change that needs durable public review.

**Published shape:**

- Summary
- Motivation
- Guide-level explanation
- Reference-level explanation
- Drawbacks
- Rationale and alternatives
- Prior art
- Unresolved questions
- Future possibilities

**Process:** Review occurs on a repository pull request. Relevant teams own the decision, and a bot-managed final comment period gathers explicit sign-off before disposition. Accepted RFCs receive durable numbers.

Sources: [Rust RFC process](https://rust-lang.github.io/rfcs/0002-rfc-process.html), [Rust RFC template](https://github.com/rust-lang/rfcs/blob/master/0000-template.md)

### Go proposal and design document

**Fits:** A change that may be cheap to triage before anyone writes a full design.

**Published approach:** Start with a proposal issue. The proposal review may accept it, decline it, or request a design document. When requested, the design document explains the problem, goals, design, rationale, compatibility, implementation implications, and alternatives at the level the proposal requires.

**Process:** A weekly proposal review tracks decisions publicly. The two-stage gate prevents unnecessary design work.

Sources: [Go proposal process](https://go.googlesource.com/proposal/+/refs/heads/master/README.md), [Go design template](https://go.googlesource.com/proposal/+/refs/heads/master/design/TEMPLATE.md)

### Kubernetes Enhancement Proposal

**Fits:** Infrastructure or platform work that must remain useful through implementation, rollout, and graduation.

**Published shape includes:**

- Summary
- Motivation, goals, and non-goals
- Proposal, user stories, risks, and mitigations
- Design details and test plan
- Graduation criteria
- Upgrade, downgrade, and version-skew strategy
- Production-readiness questions
- Drawbacks, alternatives, and infrastructure needs

**Process:** KEPs progress through states such as Provisional, Implementable, and Implemented. The format is intentionally heavier than a general RFC.

Sources: [KEP process](https://github.com/kubernetes/enhancements/blob/master/keps/sig-architecture/0000-kep-process/README.md), [KEP template](https://github.com/kubernetes/enhancements/tree/master/keps/NNNN-kep-template)

## Governance practices

### Squarespace: explicit feedback stage and “yes, if”

Squarespace distinguishes named approvers from other reviewers, uses free-text status to say what feedback the author needs now, and frames conditional approval as “yes, if.” Architecture Review provides deep feedback while Infrastructure Council provides broad visibility.

Use this process pattern when authors cannot tell what kind of feedback is wanted or when review is complete. It is governance guidance, not a universal RFC outline.

Source: [The Power of “Yes, if”](https://engineering.squarespace.com/blog/2019/the-power-of-yes-if)
