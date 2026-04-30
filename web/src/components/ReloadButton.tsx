import { useState } from 'preact/hooks';

interface ReloadButtonProps {
  onConfirm: () => void | Promise<void>;
  disabled?: boolean;
}

export function ReloadButton({ onConfirm, disabled }: ReloadButtonProps) {
  const [open, setOpen] = useState(false);
  const [pending, setPending] = useState(false);

  async function confirm() {
    setPending(true);
    try {
      await onConfirm();
    } finally {
      setPending(false);
      setOpen(false);
    }
  }

  return (
    <>
      <button type="button" class="wb-cta wb-cta--ghost" disabled={disabled || pending} aria-haspopup="dialog" onClick={() => setOpen(true)}>
        reload hooks
      </button>
      {open ? (
        <div role="dialog" aria-modal="true" aria-label="Reload hooks" class="fixed inset-0 z-50 grid place-items-center bg-black/55 p-4">
          <div class="wb-floating-panel w-full max-w-[460px] p-5">
            <h2 class="m-0 text-[20px]">reload hooks?</h2>
            <p class="mt-3 text-text-secondary">Re-reads the hooks YAML, rebuilds dispatcher bindings, and clears auto-disable state. In-flight invocations finish against the old bindings.</p>
            <div class="mt-5 flex flex-wrap justify-end gap-2">
              <button type="button" class="wb-cta wb-cta--ghost" onClick={() => setOpen(false)} disabled={pending}>cancel</button>
              <button type="button" class="wb-cta wb-cta--primary" onClick={() => void confirm()} disabled={pending}>{pending ? 'reloading...' : 'reload'}</button>
            </div>
          </div>
        </div>
      ) : null}
    </>
  );
}

export default ReloadButton;
