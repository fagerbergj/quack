import type { Context } from '@earendil-works/chord/context';
import type { ExecutionError, Result, ShellExecOptions, ShellExecResult } from '@earendil-works/pi-durable/env';
import { NodeExecutionEnv } from '@earendil-works/pi-durable/env/node';

/** Per deployment: how agent shells are confined. Limits are per process, in MB (maxProcs a count); 0 turns one off. */
export type ExecPolicy = {
  /** Variable names agent shells may see beyond the toolchain defaults. */
  env?: string[];
  /** Cap on one command; the model may ask for less. */
  timeoutSeconds?: number;
  limits?: { addressSpaceMb?: number; maxProcs?: number; maxFileSizeMb?: number };
};

export const DEFAULT_EXEC_TIMEOUT_SECONDS = 600;
// Address space and maxProcs are opt-in: V8 and WebAssembly reserve huge virtual regions, and RLIMIT_NPROC counts every process of the UID.
export const DEFAULT_EXEC_LIMITS = { maxFileSizeMb: 1024 };

const SHELL_ENV = new Set(['PATH', 'HOME', 'TMPDIR', 'TZ', 'LANG', 'LANGUAGE', 'GOPATH', 'GOMODCACHE', 'GOCACHE', 'GOFLAGS', 'CARGO_HOME', 'RUSTUP_HOME', 'NVM_DIR']);

/** The variables a shell gets from the session process: a fixed toolchain set, LC_*, and what the policy names. */
export function shellEnv(source: NodeJS.ProcessEnv, extra: string[] = []): Record<string, string> {
  const allowed = (name: string) => SHELL_ENV.has(name) || name.startsWith('LC_') || extra.includes(name);
  return Object.fromEntries(Object.entries(source).filter((e): e is [string, string] => e[1] !== undefined && allowed(e[0])));
}

/** `ulimit` lines for a bash command; a limit the inherited hard one forbids is left as it was. */
export function limitPrefix(limits: NonNullable<ExecPolicy['limits']>): string {
  const set = (flag: string, n: number | undefined, unit = 1) => (n ? `ulimit -${flag} ${n * unit} 2>/dev/null\n` : '');
  return set('v', limits.addressSpaceMb, 1024) + set('f', limits.maxFileSizeMb, 1024) + set('u', limits.maxProcs);
}

/** The one place agent shells start: a scrubbed environment, a timeout cap and resource limits on every command. */
export class PolicyExecutionEnv extends NodeExecutionEnv {
  private policy: ExecPolicy;
  private source: NodeJS.ProcessEnv;

  constructor(options: { cwd: string; policy?: ExecPolicy; source?: NodeJS.ProcessEnv }) {
    super({ cwd: options.cwd });
    this.policy = options.policy ?? {};
    this.source = options.source ?? process.env;
  }

  override async exec(command: string | readonly string[], options: ShellExecOptions | undefined, context: Context): Promise<Result<ShellExecResult, ExecutionError>> {
    const cap = this.policy.timeoutSeconds ?? DEFAULT_EXEC_TIMEOUT_SECONDS;
    const timeout = Math.min(options?.timeout ?? cap, cap);
    const limits = limitPrefix(this.policy.limits ?? DEFAULT_EXEC_LIMITS);
    // An argv form has no shell to carry ulimit, so it runs through bash, which execs it.
    const wrapped = typeof command === 'string' ? limits + command : ['bash', '-c', `${limits}exec "$@"`, 'bash', ...command];
    const env = { ...shellEnv(this.source, this.policy.env), ...options?.env };
    const result = await super.exec(wrapped, { ...options, timeout, inheritEnv: false, env }, context);
    // The bash tool words a timeout from the model's own argument, which is absent when the cap fired.
    if (!result.ok && result.error.code === 'timeout') options?.onOutput?.(`\n[command killed: exceeded the ${timeout} second timeout]\n`, context, { stream: 'stderr' });
    return result;
  }
}
