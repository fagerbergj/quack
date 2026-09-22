# Trust gate

Limited local models bluff, so nothing a node's worker produces is trusted by default. Every gated agent's output passes through `vetting.RunGatedRefine` (`internal/vetting/node.go`) before it flows downstream. The gate runs cheapest-first, each stage bounded by its own round budget - a stage with `max_rounds: 0` is skipped entirely:

```yaml
gates:
  constitution_path: config/constitution.md   # global principles, used by the judge
  rubric_path: config/rubric.md               # default scoring guide (an agent's own rubric.yaml wins)
  deterministic_checks:
    max_rounds: 4   # free citation/length checks; up to 4 cheap worker revise cycles
  judge:
    provider: default
    model: ${QUACK_JUDGE_MODEL}   # empty ⇒ judge disabled
    max_rounds: 1
    threshold: 0.7
    max_iterations: 6
    context_window: 65536
    # max_output_tokens: 8192   # caps the judge round's own reply tokens; 0 = uncapped
    # thinking_level: low      # low|medium|high - capped reasoning effort, opt-in
```

If both `deterministic_checks.max_rounds` and the judge are off, the gate is disabled entirely and agents are served unwrapped (`GatesConfig.Enabled`).

## 1. Deterministic checks

Free, mechanical checks that drive cheap targeted revisions before anything expensive runs: citation backing, length, and - for a code-implementer node - the repo's own build/vet/test commands. `deterministic_checks.max_rounds` caps how many revise cycles these checks alone can trigger.

`artifact_valid` is one of these checks, and only applies to a node whose declared artifact kind has a schema registered by an SDK extension (`sdk.ArtifactSchemas`, see [agent-plugins.md](../agent-plugins.md)). It passes only when the node's artifact for this run exists and satisfies that schema; on failure the criterion's feedback carries the same violation list a schema-refused write would show the worker, so the revise round has something concrete to fix. A node whose kind has no registered schema never gets this criterion at all.

The check commands themselves come from `workspace.check_commands` - an allowlist of command **prefixes** the planner may complete into a node's `checks` (e.g. `go build`, `npm test`). When the planner sets none, the gate derives them from that same allowlist, each further gated on the binary actually existing on the host (so a runtime without `go`/`npm` just derives nothing instead of failing nodes). They run via the shared jailed pipeline runner, `workspace.RunPipeline`, inside the node's own workdir - see [workspace](workspace/index.md).

## 2. Independent judge

A separate, independently-configured model scores the answer G-Eval style against the rubric. `provider`/`model` are set here, deliberately apart from any worker's model - see [models.md](models.md#the-judge-is-a-separate-model) for why that independence matters. Empty `model` (or `max_rounds: 0`) disables the judge; the cheaper deterministic stage still runs on its own.

Every judge round also gets `list_artifacts`/`read_artifact`, scoped to the node's chat, alongside any jail-scoped repo read tools - so an answer that points at an artifact instead of restating it (`read_artifact to see it`, a revision number) can actually be checked. A PASS that never read the repo, or never read an artifact the worker wrote or edited that round, is discarded and the round is re-judged once. Each of those two discard rules can fire at most once per round, so one `max_rounds` round costs at most 3 transient-fault attempts, an image-strip retry, a no-verdict retry, and one re-judge - bounded, never an unbounded loop.

- `threshold` (default `0.7`) is a **per-criterion** pass bar, not an average - every rubric criterion must individually clear it. The verdict score is the *lowest* criterion (weakest-link gating; no averaging, no caps).
- `max_rounds` bounds judge/revise cycles - the worker gets self-contained feedback and another attempt, up to this many times.
- `max_iterations` caps the judge's own agentic model turns within a single round (it may call tools to verify claims, e.g. reading the clone).
- `context_window` budgets the assembled judge prompt so it fits before the call, instead of discovering a 400 mid-request.
- `thinking_level` (`low`/`medium`/`high`, unset by default) opts the judge/plan-judge request into a capped reasoning effort so thinking can't consume the whole `max_output_tokens` budget before a verdict is reached - leave it unset for a non-reasoning model or an endpoint that 400s on `reasoning_effort`.

## Cut-off answers

A worker round that ends with `finish_reason: MAX_TOKENS` is never judged as-is - the answer was cut off mid-sentence, not finished. The gate runs an automatic continuation turn in the same session (quoting the answer's trailing ~200 characters so the worker resumes instead of restarting), up to `maxTruncationContinuations` (2, a fixed constant) times per round. If the answer is still cut off after that budget, the round is judged with a deterministic `complete_output` criterion scored 0, so a truncated answer can never pass by weakest-link scoring regardless of what the judge would have scored the visible text.

## Judge replay

`quack judge replay <chat-id-or-bundle.zip>` re-grades a recorded chat's already-judged rounds under the working-copy rubric, without re-running the agent or the original judge call - the fast loop for proving a rubric or gate-code change against real traffic (see [`docs/design/judging.md`](../design/judging.md)). It loads the recording the same way `quack eval`/`quack ledger export` do, then calls the gate's own `computeDeterministicCriteria`/`mergeDeterministic`/`applyRubricSpecs` and (unless `--deterministic-only`) the judge model - never a reimplementation of judging.

Pass/fail is decided by `gates.judge.threshold` from the local `quack.yaml` - the same bar `buildEnvelope` gates on live - for both the recorded and the replayed score. Each criterion's own `scale.pass` from the rubric is printed alongside as `rubric_mark`, informational only: the live gate does not read it. Beyond a plain pass/fail change on a criterion both sides have, two more flip classes:

- A criterion the live gate recorded but replay never reproduced flips only when the judge actually ran this replay (`--deterministic-only`, or no real artifact access, naturally lack judge-scored criteria - expected, not a defect) - unless it names one of `vetting.EnvironmentOnlyCriteria` (`checks_pass`, `no_vacuous_tests`, `delivery_complete`, `review_posted`, `behaviour_verified`, `artifact_valid`), which needs a live workspace/checks/reviewer/artifact context replay never has, so it is always reported informational, judge or not.
- A criterion the working-copy rubric adds that replay now computes and fails is its own flip: weakest-link would drag the round's verdict down in a way the original run never saw.

Replay's exit code: 0 when every replayed round matched; 1 when a round was skipped (no local agent config for it, or no rebuildable answer), errored, or a --node/--round filter matched nothing; 2 when any criterion's pass/fail flipped. A flip outranks a skip, so read the per-round lines, not only the code.

It rebuilds, per judged round: the node's own task (the built prompt actually sent to the worker, which live also uses as the judge's question) and the answer that round graded, from whichever of the node's own rounds chronologically precedes it - not assumed from the round-id string, since a hitl/confirm/continuation/revise/queued-retry round takes several different id forms. Worker activity (fetched/seen URLs, workspace ops) is rebuilt from every stream in the WHOLE recorded chat session (not just the judged node's own), replayed through a fresh in-memory session and scanned with the gate's own `activityFromSessionAt` - matching the live gate's `ctx.Session()` scan, which is chat-scoped, not node-scoped. `--rubric <path>` overrides the graded node's own bundled `rubric.yaml`, and its rendered text reaches `cfg.Rubric`, so an edited criterion definition actually reaches the judge prompt.

What it approximates or cannot rebuild:

- The judge's read access: with `--from-server`, the judge gets real `list_artifacts`/`read_artifact` over that server's REST API. A local bundle file has no server to read from, so the judge instead gets a stub pair that always answers "no artifacts are available" - the live judge prompt advertises these tools unconditionally, so a compliant judge must find them registered rather than hit an unknown-tool error - and a round whose own activity wrote an artifact is still replayed deterministic-only with a note, since the stub can't actually answer for it.
- Recorded scores for a (node, round) label are taken from its latest run: a queued re-run or a reused node id records a second run's criteria under the same label AND the same `ResponseID` (live passes the round id as the response id), so replay keeps only the `eval.score` entries stamped at or after the judge stream's last chat turn, which the latest run produced. The task text is not deduplicated the same way: for a re-run sharing a node id, it comes from whichever round ran earliest and may lack that later run's own queued-message section.
- A live terminal round that failed a deterministic criterion never ran the judge and recorded no judge criteria; replaying it with the judge therefore reports every failing judge criterion as "added by the working-copy rubric".
- Repo probes (`augmentFromRepo`), the review/PR staging augmentations (`augmentFromReviewStage`/`augmentFromPRStage`), and session compaction are not replayed - they need a live clone/workspace the recording doesn't carry.
- `cfg.Task` (normally the planner's raw node task) is approximated from the same built-prompt text used as the question, since the recording does not separately preserve the raw task.
- It is read-only: it never writes to the source chat, its artifacts, or its ledger.

## Rubrics

`rubric_path` is the default scoring guide; an agent's own bundle can override it with a `rubric.yaml` sitting next to its `prompt.md` (see [agents.md](agents.md)). `constitution_path` is the fixed, standing set of principles layered under every rubric - grounded claims, no fabrication - that no per-node rubric can remove.

The judge needs concrete criteria to score against, and a vague rubric makes a small judge wander. So the **planner writes a per-node rubric when it builds the DAG** - it already defines the task, and "done" is the other half:

```yaml
node:
  task: "Find the best months to visit Dublin with typical temperatures."
  rubric:
    - "States specific months, not just 'summer'"
    - "Gives a temperature range with units"
    - "Every weather claim is attributed to a retrieved source"
```

Independence still holds: the planner writes the rubric, a different model does the work, and a third (the judge) scores it.

## Dependents read the artifact

When a node's answer feeds a dependent node's prompt (`dag.buildTask`), the dependent keeps that answer exactly as before and, when the node's own artifact revision differs from it, gets that artifact appended after it under a header naming its id and revision - the answer may be a pointer or a summary ("fixed in artifact revision 3") rather than the deliverable itself. `vetting.DependencyArtifact` scopes this by `Lineage.NodeID` (never a sibling's revision under the same chat-scoped typed id) and picks the highest `Lineage.Round` across the node's configured `Artifact` kind and the generic per-round `text:<nodeID>` fallback, so a later round's tool-write is never shadowed by an earlier, possibly gate-failed round. A System kind (e.g. `web_page`) is never a candidate. Total appended bytes per task are capped to a share of the dependent's own `context_window` (`artifactContextShare`, `artifactBytesPerToken`); an oversized artifact is truncated with a trailing marker naming the id so the node can `read_artifact` the rest.

## Delivery

For a code-implementer node, the gate - not the worker - owns delivery: `commitDelivery` pushes the work branch and opens the PR exactly once, after the gate is satisfied. A gate-failed node still opens its PR, but as a draft, so a human reviewer can see what was attempted without it looking like a finished, self-approved change.

On a GitHub-triggered run, `commitDelivery` is also where the trigger's computed permission grant (see [extensions/github.md](../extensions/github.md#permissions-the-grant)) is actually enforced: it's the one place a run can reach GitHub at all, so a staged item outside the grant - a review on a run never granted `post_review`, say - is refused there regardless of what the plan declared or the worker staged. The refusal is loud: logged at error level and reported as a failed delivery, never a silent drop.
