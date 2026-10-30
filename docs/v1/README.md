# Quack v1 design

The rewrite's requirements and design, in the order they are signed off. No code is written until all of them are. All three are signed off as of 2026-10-30.

1. **Requirements** (signed off 2026-10-30): [PRD](prd.md) (why and what), [SRS](srs.md) (core requirements), and one short document per extension in [extensions/](extensions/). All of them trace to [decisions.md](decisions.md), the log of settled answers.
2. **Architecture** (signed off 2026-10-30): [RFC 0001](rfc/0001-core.md) for core and one RFC per extension in [rfc/extensions/](rfc/extensions/), built on [cloud-agent-layers.svg](cloud-agent-layers.svg).
3. **UI mockups** (signed off 2026-10-30): [mockups/whimsical.html](mockups/whimsical.html) is the approved design (published at https://claude.ai/artifact/NR45F1ypJX1WrpaQKbBwLy). [mockups/index.html](mockups/index.html) is the plainer v3 it grew from. Design rules: sessions workspace as home; mallard palette; whimsy in visuals, motion and helper notes only, never in control labels or control icons.
