import type { ComponentChildren } from 'preact';

type GlyphKind = 'idle' | 'sessions' | 'hooks' | 'missing' | 'timeline' | 'loading';

interface EmptyStateProps {
  glyph?: GlyphKind;
  title: string;
  copy: ComponentChildren;
  cta?: ComponentChildren;
}

function Glyph({ kind = 'idle' }: { kind?: GlyphKind }) {
  return (
    <svg viewBox="0 0 64 64" class="wb-empty__glyph" aria-hidden="true">
      <rect x="8" y="10" width="48" height="44" rx="4" class="wb-empty__glyph-stroke" />
      {kind === 'idle' && (
        <>
          <path d="M18 24h10v16H18zM36 20h10v20H36z" class="wb-empty__glyph-fill" />
          <path d="M16 46h32" class="wb-empty__glyph-stroke" />
        </>
      )}
      {kind === 'sessions' && (
        <>
          <path d="M18 22h28M18 32h20M18 42h16" class="wb-empty__glyph-stroke" />
          <circle cx="46" cy="42" r="6" class="wb-empty__glyph-accent" />
        </>
      )}
      {kind === 'hooks' && (
        <>
          <path d="M18 24h12v16H18zM34 18h12v28H34z" class="wb-empty__glyph-stroke" />
          <path d="M24 20v-6m16 8v-8" class="wb-empty__glyph-accent" />
        </>
      )}
      {kind === 'missing' && (
        <>
          <path d="M20 20l24 24M44 20 20 44" class="wb-empty__glyph-accent" />
        </>
      )}
      {kind === 'timeline' && (
        <>
          <path d="M20 18v28" class="wb-empty__glyph-stroke" />
          <circle cx="20" cy="24" r="3" class="wb-empty__glyph-fill" />
          <circle cx="20" cy="36" r="3" class="wb-empty__glyph-fill" />
          <path d="M28 24h16M28 36h12" class="wb-empty__glyph-stroke" />
        </>
      )}
      {kind === 'loading' && (
        <>
          <circle cx="32" cy="32" r="10" class="wb-empty__glyph-stroke" />
          <path d="M32 20a12 12 0 0 1 12 12" class="wb-empty__glyph-accent" />
        </>
      )}
    </svg>
  );
}

export function EmptyState({ glyph = 'idle', title, copy, cta }: EmptyStateProps) {
  return (
    <div class="wb-empty" role="status">
      <Glyph kind={glyph} />
      <h3 class="wb-empty__title">{title}</h3>
      <p class="wb-empty__copy">{copy}</p>
      {cta ? <div class="wb-empty__cta">{cta}</div> : null}
    </div>
  );
}
