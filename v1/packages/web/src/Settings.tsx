import { useState } from 'react';
import { storedTheme, THEME_KEY } from './Shell.tsx';

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

export function Settings() {
  const [choice, setChoice] = useState(() => storedTheme() ?? 'system');
  return (
    <div className="mx-auto max-w-3xl px-3 py-4 lg:py-8">
      <h1 className="m-0 mb-3 hidden px-1 text-[19px] lg:block">Settings</h1>
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
    </div>
  );
}
