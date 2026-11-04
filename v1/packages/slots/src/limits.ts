/** Slots per provider id from the deployment file's `providers.<id>.slots`; a provider without one is not gated. */
export const slotLimits = (deployment: { providers: Record<string, { slots?: number }> }): Record<string, number> =>
  Object.fromEntries(Object.entries(deployment.providers).flatMap(([id, p]) => (p.slots ? [[id, p.slots]] : [])));
