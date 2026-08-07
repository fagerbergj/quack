# Trust gate

Limited local models bluff, so nothing a node's worker produces is trusted by default. Every gated agent's output passes through `vetting.RunGatedRefine` (`internal/vetting/node.go`) before it flows downstream.
The gate runs cheapest-first, each stage bounded by its own round budget - a stage with `max_rounds: 0` is skipped entirely:

```yaml
gates:
  constitution_path: config/constitution.md   # global principles, shared by the advisor + judge
  rubric_path: config/rubric.md               # default scoring guide (an agent's own rubric.md wins)
  deterministic_checks:
    max_rounds: 4   # free citation/length checks; up to 4 cheap worker revise cycles
  judge:
    provider: default
    model: ${QUACK_JUDGE_MODEL}   # empty ⇒ judge disabled
    max_rounds: 1
    threshold: 0.7
    max_iterations: 6
    context_window: 65536
    skeptics: 0
```

If both `deterministic_checks.max_rounds` and the judge are off, the gate is disabled entirely and agents are served unwrapped (`GatesConfig.Enabled`).

## 1. Deterministic checks

Free, mechanical checks that drive cheap targeted revisions before anything expensive runs: citation backing, length, and - for a code-implementer node - the repo's own build/vet/test commands. `deterministic_checks.max_rounds` caps how many revise cycles these checks alone can trigger.

The check commands themselves come from `workspace.check_commands` - an allowlist of command **prefixes** the planner may complete into a node's `checks` (e.g. `go build`, `npm test`). When the planner sets none, the gate derives them from that same allowlist, each further gated on the binary actually existing on the host (so a runtime without `go`/`npm` just derives nothing instead of failing nodes). They run via the shared jailed pipeline runner, `workspace.RunPipeline`, inside the node's own workdir - see [workspace](workspace/index.md).

## 2. Independent judge

A separate, independently-configured model scores the answer G-Eval style against the rubric. `provider`/`model` are set here, deliberately apart from any worker's model - see [models.md](models.md#the-judge-is-a-separate-model) for why that independence matters. Empty `model` (or `max_rounds: 0`) disables the judge; the cheaper deterministic stage still runs on its own.

- `threshold` (default `0.7`) is a **per-criterion** pass bar, not an average - every rubric criterion must individually clear it. The verdict score is the *lowest* criterion (weakest-link gating; no averaging, no caps).
- `max_rounds` bounds judge/revise cycles - the worker gets self-contained feedback and another attempt, up to this many times.
- `max_iterations` caps the judge's own agentic model turns within a single round (it may call tools to verify claims, e.g. reading the clone).
- `context_window` budgets the assembled judge prompt so it fits before the call, instead of discovering a 400 mid-request.
- `skeptics` (default `0`, off) is the adversarial-verify stage: N independent skeptic calls per load-bearing *passing* criterion, with a strict majority-refute killing the finding. Each qualifying criterion costs N extra judge-model calls, so it's opt-in.

## Rubrics

`rubric_path` is the default scoring guide; an agent's own bundle can override it with a `rubric.md` sitting next to its `prompt.md` (see [agents.md](agents.md)). `constitution_path` is the fixed, standing set of principles layered under every rubric - grounded claims, no fabrication - that no per-node rubric can remove.

The judge needs concrete criteria to score against, and a vague rubric makes a small judge wander.
So the **planner writes a per-node rubric when it builds the DAG** - it already defines the task, and "done" is the other half:

```yaml
node:
  task: "Find the best months to visit Dublin with typical temperatures."
  rubric:
    - "States specific months, not just 'summer'"
    - "Gives a temperature range with units"
    - "Every weather claim is attributed to a retrieved source"
```

Independence still holds: the planner writes the rubric, a different model does the work, and a third (the judge) scores it.

## The advisor is not a gate stage

`agents/advisor` isn't one of the stages above - it's the `ask_advisor` *tool*, which a worker calls at its own discretion mid-run.
It reuses the judge's provider/model and is only wired onto a worker's tool list when the judge is enabled (leaving `gates.judge.model` empty turns off both the judge and `ask_advisor` together).

## Delivery

For a code-implementer node, the gate - not the worker - owns delivery: `commitDelivery` pushes the work branch and opens the PR exactly once, after the gate is satisfied.
A gate-failed node still opens its PR, but as a draft, so a human reviewer can see what was attempted without it looking like a finished, self-approved change.

On a GitHub-triggered run, `commitDelivery` is also where the trigger's computed permission grant (see [extensions/github.md](../extensions/github.md#permissions-the-grant)) is actually enforced: it's the one place a run can reach GitHub at all, so a staged item outside the grant - a review on a run never granted `post_review`, say - is refused there regardless of what the plan declared or the worker staged.
The refusal is loud: logged at error level and reported as a failed delivery, never a silent drop.
