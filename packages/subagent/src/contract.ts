/** The `subagent` tool: an ordinary extension an agent selects with `"extensions": { "subagent": {} }`. A child is a conversation of the calling session owned by the call, running the parent's agent minus `subagent`; viewers read its transcript through the session API by the id in the details. */

/** Arguments of the `subagent` tool. Several calls in one turn run in parallel. */
export type SubagentArgs = {
  /** The child's whole brief, non-empty. The child sees none of the parent's conversation. */
  task: string;
};

/** Details recorded on the call as it runs, so a viewer can attach to the child's conversation. */
export type SubagentDetails = {
  /** The child conversation, owned by the call. A rerun after a restart reuses it; aborting the call aborts it. */
  conversationId: string;
};
