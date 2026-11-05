/**
 * The public surface of delegation: the `subagent` tool every session has, and the limits that bound it.
 * The tool's result reaches the model as text: the child's final answer.
 */

/** The deployment file's `subagents` key. Limits are per session; each takes the default shown when absent. */
export type SubagentsConfig = {
  /** Subagents running at once. A call past it waits for one to finish. Default 4. */
  maxConcurrent?: number;
  /** Subagents a session may ever start. A call past it is refused. Default 20. */
  maxTotal?: number;
};

/** Arguments of the `subagent` tool. Several calls in one turn run in parallel. */
export type SubagentArgs = {
  /** The child's whole brief. It sees none of the parent's conversation but copies the parent's instructions and tools, minus `subagent`. */
  task: string;
  /** Another agent whose model and prompt the child takes in place of the parent's. An unknown name is refused. */
  agent?: string;
};

/** Details recorded on the call as it runs, so a viewer can attach to the child's conversation. */
export type SubagentDetails = {
  /** The child conversation, owned by the call. Aborting the call aborts it. */
  conversationId: string;
};

/** An agent definition's `subagents` key. `false` leaves the session without the `subagent` tool. */
export type AgentSubagents = false;
