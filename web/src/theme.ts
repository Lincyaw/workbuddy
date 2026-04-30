export type ThemeMode = 'system' | 'light' | 'dark';
export type ResolvedTheme = Exclude<ThemeMode, 'system'>;

export const THEME_STORAGE_KEY = 'wb:theme';

function isThemeMode(value: string | null): value is ThemeMode {
  return value === 'system' || value === 'light' || value === 'dark';
}

export function getStoredTheme(): ThemeMode {
  if (typeof window === 'undefined') return 'dark';
  const value = window.localStorage.getItem(THEME_STORAGE_KEY);
  return isThemeMode(value) ? value : 'dark';
}

export function getSystemTheme(): ResolvedTheme {
  if (typeof window === 'undefined' || typeof window.matchMedia !== 'function') {
    return 'dark';
  }
  return window.matchMedia('(prefers-color-scheme: light)').matches ? 'light' : 'dark';
}

export function resolveTheme(mode: ThemeMode): ResolvedTheme {
  return mode === 'system' ? getSystemTheme() : mode;
}

export function applyTheme(mode: ThemeMode): ResolvedTheme {
  const resolved = resolveTheme(mode);
  document.documentElement.dataset.theme = resolved;
  return resolved;
}
