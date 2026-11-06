/** Who made a decision: a person the reverse proxy authenticated, or a caller on the tailnet, which is trusted at its perimeter. */
export type Principal = { kind: 'person'; name: string } | { kind: 'tailnet' };

/** The name a decision is shown and indexed under; a row recorded before principals were typed holds that name as a bare string. */
export const principalName = (by: Principal | string): string => (typeof by === 'string' ? by : by.kind === 'person' ? by.name : 'tailnet');
