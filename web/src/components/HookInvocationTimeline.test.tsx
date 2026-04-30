import { fireEvent, render } from '@testing-library/preact';
import { describe, expect, it } from 'vitest';
import { HookInvocationTimeline } from './HookInvocationTimeline';
import type { HookInvocation } from '../api/hooks';

function inv(over: Partial<HookInvocation> = {}): HookInvocation {
  return {
    started_at: '2026-04-30T10:00:00Z',
    finished_at: '2026-04-30T10:00:00Z',
    duration_ms: 12,
    result: 'success',
    ...over,
  };
}

describe('HookInvocationTimeline', () => {
  it('renders a placeholder when no invocations are recorded', () => {
    const { getByTestId } = render(<HookInvocationTimeline invocations={[]} />);
    expect(getByTestId('hook-timeline-empty')).not.toBeNull();
  });

  it('renders one row per invocation with the result chip', () => {
    const { container, getByText } = render(
      <HookInvocationTimeline
        invocations={[
          inv({ result: 'success', event_type: 'alert' }),
          inv({ result: 'failure', error: 'boom', event_type: 'alert' }),
          inv({ result: 'overflow' }),
        ]}
      />,
    );
    const rows = container.querySelectorAll('li.wb-hook-inv');
    expect(rows.length).toBe(3);
    expect(getByText('success')).not.toBeNull();
    expect(getByText('failure')).not.toBeNull();
    expect(getByText('overflow')).not.toBeNull();
  });

  it('expands the body with stdout / stderr previews when the row is clicked', () => {
    const { container } = render(
      <HookInvocationTimeline
        invocations={[
          inv({ stdout: 'hello world', stderr: 'oops' }),
        ]}
      />,
    );
    const button = container.querySelector('button.wb-hook-inv-row') as HTMLButtonElement;
    expect(button.getAttribute('aria-expanded')).toBe('false');
    fireEvent.click(button);
    expect(button.getAttribute('aria-expanded')).toBe('true');

    const body = container.querySelector('.wb-hook-inv-body');
    expect(body).not.toBeNull();
    expect(body?.textContent).toContain('hello world');
    expect(body?.textContent).toContain('oops');
  });

  it('collapses again when the same row is clicked twice', () => {
    const { container } = render(
      <HookInvocationTimeline
        invocations={[inv({ stdout: 'snippet' })]}
      />,
    );
    const button = container.querySelector('button.wb-hook-inv-row') as HTMLButtonElement;
    fireEvent.click(button);
    fireEvent.click(button);
    expect(button.getAttribute('aria-expanded')).toBe('false');
    expect(container.querySelector('.wb-hook-inv-body')).toBeNull();
  });

  it('shows the error string in the expanded body of a failure', () => {
    const { container, getByText } = render(
      <HookInvocationTimeline invocations={[inv({ result: 'failure', error: 'kaboom' })]} />,
    );
    fireEvent.click(container.querySelector('button.wb-hook-inv-row') as HTMLButtonElement);
    expect(getByText('kaboom')).not.toBeNull();
  });

  it('shows a "no captured output" hint when stdout / stderr / error are all empty', () => {
    const { container, getByText } = render(
      <HookInvocationTimeline invocations={[inv({ result: 'overflow' })]} />,
    );
    fireEvent.click(container.querySelector('button.wb-hook-inv-row') as HTMLButtonElement);
    expect(getByText('No captured output.')).not.toBeNull();
  });
});
