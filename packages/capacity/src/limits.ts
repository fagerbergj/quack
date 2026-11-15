/** Capacity per provider id from the deployment file's `providers.<id>.capacity`; a provider without one is not gated. */
export const capacityLimits = (deployment: { providers: Record<string, { capacity?: number }> }): Record<string, number> =>
  Object.fromEntries(Object.entries(deployment.providers).flatMap(([id, p]) => (p.capacity ? [[id, p.capacity]] : [])));
