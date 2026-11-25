/** Who made a decision: a person the reverse proxy authenticated, a caller on the tailnet, which is trusted at its perimeter, or the
 * approval.approving checks, which turn a person's approve into a rejection when they find problems. */
export type Principal = { kind: 'person'; name: string } | { kind: 'tailnet' } | { kind: 'checks' };

/** The name a decision is shown and indexed under; a row recorded before principals were typed holds that name as a bare string. */
export const principalName = (by: Principal | string): string => (typeof by === 'string' ? by : by.kind === 'person' ? by.name : by.kind === 'checks' ? 'the checks' : 'tailnet');
