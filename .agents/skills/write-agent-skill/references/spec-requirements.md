# Agent Skills Specification - Detailed Requirements

Load this file when auditing a skill for spec compliance or when the validation checklist in `templates/skill-template.md` raises a question about a specific field constraint.

---

## 1. Directory Structure

Every skill **must** be a directory containing at minimum a `SKILL.md` file:

```
skill-name/
├── SKILL.md          # Required
├── scripts/          # Optional: executable code (Python, Bash, JS)
├── references/       # Optional: on-demand documentation
└── assets/           # Optional: templates, diagrams, data files, schemas
```

Reference files must be **one level deep** from `SKILL.md`. Never point a reference file to another reference file.

---

## 2. Frontmatter - Exact Field Constraints

| Field | Required | Max Length | Constraints |
|---|---|---|---|
| `name` | Yes | 64 chars | Lowercase `a-z`, `0-9`, hyphens only. No leading/trailing `-`. No `--`. Must match parent directory name exactly. |
| `description` | Yes | 1,024 chars | Non-empty. Covers what the skill does **and** when to use it. Includes specific trigger keywords. |
| `license` | No | - | Short license name or reference to a bundled file. |
| `compatibility` | No | 500 chars | Environment requirements only. Omit unless the skill has real requirements. |
| `metadata` | No | - | Flat string key-value map. Keys should be reasonably unique. |
| `allowed-tools` | No | - | **Experimental.** Space-separated tool names (e.g. `Bash(git:*) Read Write`). Support varies by implementation. |

### Valid / Invalid `name` Examples

```yaml
# Valid
name: pdf-processing
name: write-agent-skill

# Invalid - uppercase
name: PDF-Processing

# Invalid - leading hyphen
name: -pdf

# Invalid - consecutive hyphens
name: pdf--processing
```

### `description` Quality

| Good | Poor |
|---|---|
| `Extracts text and tables from PDF files. Use when the user mentions PDFs, forms, or document extraction.` | `Helps with PDFs.` |

---

## 3. Body Length Limits

| Tier | Budget | When Loaded |
|---|---|---|
| Metadata (`name` + `description`) | ~100 tokens | Always, at startup |
| Body (SKILL.md) | **<500 lines / ~5,000 tokens** | On activation |
| Resources (`references/`, `assets/`, `scripts/`) | As needed | On demand only |

---

## 4. File References

- Use relative paths from the skill root.
- One level deep maximum.
- Every reference must include a **trigger condition** - when to load it, not just that it exists.

```markdown
# Good
Read `references/api-errors.md` if the API returns a status code ≥ 400.

# Poor
See references/ for details.
```

---

## 5. Types of Skills

**Domain Knowledge** - packages specialized expertise. SKILL.md holds the chosen approach and key patterns; references hold exhaustive deep-dives.

**Workflow Pattern** - reads like a playbook with pre-flight checklist, staged steps with thresholds, rollback criteria, and pointers to detailed runbooks.

**Utility Script** - bundles executable scripts. The agent runs them and receives concise output without loading source into context.

---

## 6. Validation Command

```bash
skills-ref validate ./<skill-name>
```

Checks frontmatter validity and naming conventions. Fix reported issues and revalidate before delivering.

---

## Sources

- [agentskills.io/specification](https://agentskills.io/specification) - Authoritative field constraints, directory structure, length limits, file referencing rules, validation command.
- [agentskills.io/skill-creation/best-practices](https://agentskills.io/skill-creation/best-practices) - Scoping, calibration, defaults-not-menus, procedures-over-declarations, gotchas, validation loops.
