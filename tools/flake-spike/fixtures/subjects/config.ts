let cached: { ttl: number } | undefined;

/** The process config, read from the environment once and kept for the process's life. */
export function config(): { ttl: number } {
  cached ??= { ttl: Number(process.env.CACHE_TTL ?? 60) };
  return cached;
}
