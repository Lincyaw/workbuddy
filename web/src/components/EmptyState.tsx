import type { ComponentChildren } from 'preact';

interface EmptyStateProps {
  title: string;
  detail: string;
  ctaHref?: string;
  ctaLabel?: string;
  inline?: boolean;
  children?: ComponentChildren;
}

export function EmptyState({
  title,
  detail,
  ctaHref,
  ctaLabel,
  inline = false,
  children,
}: EmptyStateProps) {
  return (
    <div class={`empty-state${inline ? ' empty-state-inline' : ''}`} role="status">
      <svg viewBox="0 0 64 64" aria-hidden="true" class="empty-state-glyph">
        <rect x="8" y="10" width="48" height="44" rx="4" />
        <path d="M18 22h28M18 30h20M18 38h14" />
        <path d="M42 40l8 8" />
        <path d="M40 46l-2-8 8 2" />
      </svg>
      <div class="empty-state-copy">
        <h2>{title}</h2>
        <p>{detail}</p>
        {children}
        {ctaHref && ctaLabel ? (
          <a href={ctaHref} class="wb-button wb-button-primary">
            {ctaLabel}
          </a>
        ) : null}
      </div>
    </div>
  );
}
