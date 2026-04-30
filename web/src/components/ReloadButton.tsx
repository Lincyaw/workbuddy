import { useState } from 'preact/hooks';

interface ReloadButtonProps {
  onConfirm: () => void | Promise<void>;
  disabled?: boolean;
}

// ReloadButton is the top-of-page Reload control for the hooks pages. The
// dialog is intentionally a vanilla in-flow modal — the surrounding Layout
// is small enough that we don't need an overlay manager.
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
        class="wb-reload-btn"
        disabled={disabled || pending}
        aria-haspopup="dialog"
        onClick={() => setOpen(true)}
      >
        Reload
      </button>
      {open && (
        <div role="dialog" aria-modal="true" aria-label="Reload hooks" class="wb-reload-dialog">
          <div class="wb-reload-dialog-card">
            <h2>Reload hooks?</h2>
            <p class="muted">
              Re-reads the hooks YAML, rebuilds dispatcher bindings, and clears
              auto-disable state. In-flight invocations finish against the old
              bindings.
            </p>
            <div class="wb-reload-dialog-actions">
              <button
                type="button"
                onClick={() => setOpen(false)}
                disabled={pending}
              >
                Cancel
              </button>
              <button
                type="button"
                class="primary"
                onClick={() => {
                  void confirm();
                }}
                disabled={pending}
              >
                {pending ? 'Reloading…' : 'Reload'}
              </button>
            </div>
          </div>
        </div>
      )}
    </>
  );
}

export default ReloadButton;
