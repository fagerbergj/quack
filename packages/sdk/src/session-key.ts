/** The session-key grammar (contracts:20207648 v4): 1 to 200 of A-Z a-z 0-9 : / # . _ @ -. */
export const SESSION_KEY = /^[A-Za-z0-9:/#._@-]{1,200}$/;

export const isSessionKey = (key: string): boolean => SESSION_KEY.test(key);
