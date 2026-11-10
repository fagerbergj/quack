/** Fetches a URL and reports whether it answered 2xx. */
export async function up(url: string, f: typeof fetch = fetch): Promise<boolean> {
  try {
    return (await f(url)).ok;
  } catch {
    return false;
  }
}
