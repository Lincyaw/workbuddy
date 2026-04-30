import type { Config } from 'tailwindcss';

export default {
  content: ['./index.html', './src/**/*.{ts,tsx}'],
  theme: {
    extend: {
      colors: {
        'bg-canvas': 'var(--bg-canvas)',
        'bg-panel': 'var(--bg-panel)',
        'bg-elev': 'var(--bg-elev)',
        'border-hairline': 'var(--border-hairline)',
        'border-strong': 'var(--border-strong)',
        'text-primary': 'var(--text-primary)',
        'text-secondary': 'var(--text-secondary)',
        'text-tertiary': 'var(--text-tertiary)',
        accent: 'var(--accent)',
        'accent-glow': 'var(--accent-glow)',
        'state-running': 'var(--state-running)',
        'state-success': 'var(--state-success)',
        'state-warning': 'var(--state-warning)',
        'state-danger': 'var(--state-danger)',
        'state-neutral': 'var(--state-neutral)',
      },
      fontFamily: {
        sans: ['"Geist Sans"', 'sans-serif'],
        mono: ['"JetBrains Mono"', 'monospace'],
      },
      borderRadius: {
        chip: '2px',
        panel: '4px',
      },
      boxShadow: {
        float: '0 12px 40px rgba(0, 0, 0, 0.5), inset 0 1px 0 #2c333f',
      },
    },
  },
} satisfies Config;
