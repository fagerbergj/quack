export type User = { email: string; name: string; due: number };
export type Deps = { send(to: string, body: string): Promise<void>; now(): number };

/** Reminds users whose due time has passed; others are skipped. */
export function createNotifier(deps: Deps) {
  return {
    async remind(user: User): Promise<'sent' | 'skipped'> {
      if (user.due > deps.now()) return 'skipped';
      await deps.send(user.email, `Hi ${user.name}, your invoice is overdue.`);
      return 'sent';
    },
  };
}
