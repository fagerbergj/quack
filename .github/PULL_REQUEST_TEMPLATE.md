<!-- Conventional-commit title: feat(scope): … / fix(scope): … / docs(scope): … -->

## What & why

<!-- One or two lines: what changed and the motivation. The story/incident goes here, not in the source. -->

Closes #

## Evidence

<!-- Optional. Green CI is the bar; add a replay, A/B run or QA rig run here only when you ran one that helps a reviewer decide. -->

## Checklist

- [ ] `npm run typecheck` and `npm test` pass
- [ ] `npm run sloplint -- diff origin/<base>` is clean (run after committing)
- [ ] A changed agent bundle has a version bump and a rewritten `version.lock`
- [ ] Comments say what the code *can't* - no narration or change history
