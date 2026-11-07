export const sleepFor = (ms: number) => new Promise<void>((r) => setTimeout(r, ms));
