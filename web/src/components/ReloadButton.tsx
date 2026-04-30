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
      <button
        type="button"
        class="wb-button wb-button--secondary"
        disabled={disabled || pending}
        aria-haspopup="dialog"
        onClick={() => setOpen(true)}
      >
        Reload hooks
      </button>
      {open && (
        <div role="dialog" aria-modal="true" aria-label="Reload hooks" class="wb-dialog-backdrop">
          <div class="wb-dialog-card">
            <h2>Reload hooks?</h2>
            <p class="wb-muted">
              Re-reads the hooks YAML, rebuilds dispatcher bindings, and clears auto-disable state. In-flight invocations finish against the old bindings.
            </p>
            <div class="wb-dialog-actions">
              <button type="button" class="wb-button wb-button--ghost" onClick={() => setOpen(false)} disabled={pending}>
                Cancel
              </button>
              <button
                type="button"
                class="wb-button wb-button--primary"
                onClick={() => {
                  void confirm();
                }}
                disabled={pending}
              >
                {pending ? 'Reloading...' : 'Reload'}
              </button>
            </div>
          </div>
        </div>
      )}
    </>
  );
}

export default ReloadButton;
