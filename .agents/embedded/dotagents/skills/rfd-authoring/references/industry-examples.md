# RFD industry examples

RFD is not a standardized artifact. The examples below deliberately show different meanings. Use the local definition when one exists.

## Published RFD practices

### Nebari: proposal, discussion, and vote

**Fits:** A major feature, sub-project, or workflow change affecting core users or the community.

**Published content:** Nebari asks the author to open a governance issue describing the proposal’s details, benefits, impact, and other information required by its issue template. It does not publish a stable Markdown section outline on the process page.

**Process:** The issue is labeled for discussion, the core team and relevant people are tagged, and the author answers questions and objections. Once comments are addressed, the RFD moves to a vote. Approval requires more than 50% yes votes; the issue closes after implementation.

This is a concrete proposal process, not merely pre-proposal exploration.

Source: [Nebari decision making](https://nebari.dev/community/decision-making/)

### LSST DM: scheduled technical discussion

**Fits:** Detailed component or interface design, design review, brainstorming, or sharing design knowledge.

**Published content:** The RFD is a tracked issue whose description summarizes the topic and desired outcome, supplies background material, and links the meeting location. It identifies the relevant component and discussion time. The process page does not define a larger section template.

**Process:** The issue notifies the engineering community and schedules an in-depth discussion. Resulting work tickets or RFCs link back to the RFD.

This model treats the document as durable framing for a synchronous discussion.

Source: [LSST discussion and decision-making process](https://developer.lsst.io/v/contributing/processes/decision_process.html)

## Related processes that use other names

### IETF Internet-Drafts

The IETF does not use RFD as a standard artifact. Internet-Drafts are works in progress that may become RFCs, while published RFCs are immutable. Do not borrow IETF terminology for an internal RFD without adopting its actual process.

Sources: [IETF RFC series](https://www.rfc-editor.org/series/rfc/), [Bringing new work to the IETF](https://www.ietf.org/process/new-work/)

### Kubernetes Provisional KEPs

Kubernetes uses KEPs rather than RFDs. The Provisional state allows exploration before the proposal becomes Implementable, with stronger design and readiness requirements added over time. This is an example of representing uncertainty as a lifecycle state instead of a separate document type.

Source: [KEP process](https://github.com/kubernetes/enhancements/blob/master/keps/sig-architecture/0000-kep-process/README.md)

## House-neutral exploratory default

This is a synthesized fallback, not an industry standard. Use it only when the organization calls the artifact an RFD but provides no format:

- question or opportunity;
- problem, context, and affected people;
- desired outcome;
- goals and non-goals;
- constraints, assumptions, and unknowns;
- candidate directions without premature commitment;
- questions for discussion;
- evidence needed;
- provisional direction, if any;
- owner, discussion channel, next step, and closing rule.

Keep this lighter and less committed than an RFC. Its output is a clearer next decision, not necessarily an approved solution.
