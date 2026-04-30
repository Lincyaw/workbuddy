import type { ComponentChildren } from 'preact';

interface EmptyStateProps {
  icon: string;
  title: string;
  copy: ComponentChildren;
  cta?: ComponentChildren;
}

export function EmptyState({ icon, title, copy, cta }: EmptyStateProps) {
  return (
    <div class="wb-empty" role="status">
      <div class="wb-empty__icon" aria-hidden="true">{icon}</div>
      <h3 class="wb-empty__title">{title}</h3>
      <p class="wb-empty__copy">{copy}</p>
      {cta ? <div class="wb-empty__cta">{cta}</div> : null}
    </div>
  );
}
