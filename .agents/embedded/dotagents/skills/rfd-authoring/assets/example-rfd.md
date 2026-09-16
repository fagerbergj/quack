# Real RFD example: Switch from Terraform to OpenTofu

- **Project:** Nebari
- **Authors:** Adam-D-Lewis, dcmcand, marcelovilla, and viniciusdc
- **Status:** Accepted and implemented in Nebari 2024.12.1
- **Canonical document:** [nebari-dev/governance issue #56](https://github.com/nebari-dev/governance/issues/56)

This is a curated excerpt and structural guide, not a vendored copy. The issue page does not identify a license for redistributing its full text.

## Why this example is useful

The RFD ties a dependency decision to a concrete licensing event, explains the user benefit, links a working implementation, reports tests across local, AWS, GCP, and upgrade scenarios, and records the accepted outcome. The source also has an unfinished alternative and empty template sections, so use it as evidence of a successful decision process, not as a polished writing model.

## Published structure

- Status, authors, dates, and decision deadline
- Title
- Summary
- User benefit
- Design proposal
- Alternatives or approaches considered
- Best practices
- User impact
- Unresolved questions

## Excerpt

> OpenTofu acts as a drop-in replacement for Terraform and we just need to change the way we download and execute the binary. Changes would be relatively small and non-invasive and all the HCL files we currently have would stay the same.

The RFD then links a draft pull request and successful tests for local, AWS, GCP, and existing-deployment upgrade scenarios.

## What to learn from it

- Anchor the discussion in a specific event and present constraint.
- Explain user benefit separately from implementation convenience.
- Test the proposed direction before asking for a decision when that test is cheap.
- Link evidence instead of merely claiming compatibility.
- Preserve alternatives even when the preferred direction appears straightforward.

Read the [complete RFD and discussion](https://github.com/nebari-dev/governance/issues/56) before using it as a model.
