import { useEffect, useState } from 'react';
import { getVersion, type Version } from './api.ts';

export const useVersion = () => {
  const [v, setV] = useState<Version | null>(null);
  useEffect(() => { void getVersion().then(setV); }, []);
  return v;
};

export const shortCommit = (commit: string) => commit.slice(0, 7);
