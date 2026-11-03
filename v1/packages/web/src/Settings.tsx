import { useState } from 'react';
import { BottomBarSetting } from './BottomBarSetting.tsx';
import { NotificationsSetting } from './NotificationsSetting.tsx';
import { storedTheme, THEME_KEY } from './Shell.tsx';
import { shortCommit, useVersion } from './useVersion.ts';

const CHOICES = [
  { value: 'system', label: 'System' },
  { value: 'light', label: 'Light' },
  { value: 'dark', label: 'Dark' },
];

// System removes both the stored choice and data-theme, so color-scheme falls back to the OS.
function applyTheme(choice: string) {
  const root = document.documentElement;
  try {
    if (choice === 'system') localStorage.removeItem(THEME_KEY);
    else localStorage.setItem(THEME_KEY, choice);
  } catch {
    // Private mode or blocked storage: the choice just won't persist.
  }
  if (choice === 'system') delete root.dataset.theme;
  else root.dataset.theme = choice;
}

function About() {
  const v = useVersion();
  if (!v) return null;
  return (
    <section aria-labelledby="about" className="m-0 rounded-[18px] border border-line bg-surface p-3.5 shadow-card">
      <h2 id="about" className="m-0 px-1 text-xs font-semibold tracking-wider text-muted uppercase">About</h2>
      <p className="m-0 mt-2 px-1 text-[15px] text-ink">
        {v.commit ? (
          <>quack v1 · <a href={`https://github.com/fagerbergj/quack/commit/${v.commit}`} className="font-mono text-accent">{shortCommit(v.commit)}</a></>
        ) : 'quack v1 · development build'}
      </p>
      {v.builtAt && <p className="m-0 mt-1 px-1 text-[13px] text-muted">Built {new Date(v.builtAt).toLocaleString([], { dateStyle: 'medium', timeStyle: 'short' })}</p>}
    </section>
  );
}

export function Settings() {
  const [choice, setChoice] = useState(() => storedTheme() ?? 'system');
  return (
    <div className="mx-auto flex max-w-3xl flex-col gap-4 px-3 py-4 lg:py-8">
      <h1 className="m-0 hidden px-1 text-[19px] lg:block">Settings</h1>
      <fieldset className="m-0 min-w-0 rounded-[18px] border border-line bg-surface p-3.5 shadow-card">
        <legend className="px-1 text-xs font-semibold tracking-wider text-muted uppercase">Appearance</legend>
        {CHOICES.map((c) => (
          <label key={c.value} className="flex min-h-11 cursor-pointer items-center gap-3 rounded-lg px-2 text-[15px] text-ink hover:bg-soft">
            <input
              type="radio"
              name="theme"
              value={c.value}
              checked={choice === c.value}
              onChange={() => {
                applyTheme(c.value);
                setChoice(c.value);
              }}
              className="size-5 accent-accent"
            />
            {c.label}
          </label>
        ))}
      </fieldset>
      <NotificationsSetting />
      <BottomBarSetting />
      <About />
    </div>
  );
}
