export type ThemePreference = 'system' | 'light' | 'dark';

export const THEME_STORAGE_KEY = 'wb:theme';

export function normalizeThemePreference(value: string | null | undefined): ThemePreference {
  if (value === 'light' || value === 'dark' || value === 'system') return value;
  return 'system';
}

export function readThemePreference(): ThemePreference {
  if (typeof window === 'undefined') return 'system';
  return normalizeThemePreference(window.localStorage.getItem(THEME_STORAGE_KEY));
}

export function resolveTheme(preference: ThemePreference): 'light' | 'dark' {
  if (preference === 'light' || preference === 'dark') return preference;
  if (typeof window !== 'undefined' && window.matchMedia?.('(prefers-color-scheme: dark)').matches) {
    return 'dark';
  }
  return 'light';
}

export function applyThemePreference(preference: ThemePreference): 'light' | 'dark' {
  const resolved = resolveTheme(preference);
  if (typeof document !== 'undefined') {
    document.documentElement.dataset.theme = resolved;
    document.documentElement.dataset.themePreference = preference;
  }
  return resolved;
}

export function persistThemePreference(preference: ThemePreference): void {
  if (typeof window === 'undefined') return;
  window.localStorage.setItem(THEME_STORAGE_KEY, preference);
}

export function cycleThemePreference(current: ThemePreference): ThemePreference {
  if (current === 'system') return 'light';
  if (current === 'light') return 'dark';
  return 'system';
}
