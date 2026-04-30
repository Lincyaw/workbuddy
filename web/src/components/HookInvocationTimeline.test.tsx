import { fireEvent, render } from '@testing-library/preact';
import { describe, expect, it } from 'vitest';
import { HookInvocationTimeline } from './HookInvocationTimeline';
import type { HookInvocation } from '../api/hooks';

function inv(overrides: Partial<HookInvocation> = {}): HookInvocation {
  return {
    started_at: '2026-04-30T10:00:00Z',
    finished_at: '2026-04-30T10:00:10Z',
    duration_ms: 10,
    result: 'success',
    ...overrides,
  };
}

describe('HookInvocationTimeline', () => {
  it('renders a placeholder when no invocations exist', () => {
    const { container } = render(<HookInvocationTimeline invocations={[]} />);
    expect(container.textContent).toContain('No invocations recorded yet.');
  });

  it('expands an invocation row to show captured output', () => {
    const { container } = render(
      <HookInvocationTimeline invocations={[inv({ stdout: 'hello', stderr: 'warn' })]} />,
    );
    const row = container.querySelector('.timeline-row') as HTMLButtonElement;
    fireEvent.click(row);
    expect(container.textContent).toContain('hello');
    expect(container.textContent).toContain('warn');
  });
});
