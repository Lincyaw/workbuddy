import { fireEvent, render } from '@testing-library/preact';
import { describe, expect, it } from 'vitest';
import { HookInvocationTimeline } from './HookInvocationTimeline';

const invocation = {
  started_at: '2026-04-30T09:00:00Z',
  finished_at: '2026-04-30T09:00:01Z',
  duration_ms: 120,
  result: 'success',
  event_type: 'dispatch',
  repo: 'Lincyaw/workbuddy',
  issue_num: 287,
  stdout: 'ok',
  stderr: 'warn',
};

describe('HookInvocationTimeline', () => {
  it('renders a placeholder when no invocations are recorded', () => {
    const { getByTestId } = render(<HookInvocationTimeline invocations={[]} />);
    expect(getByTestId('hook-timeline-empty')).not.toBeNull();
  });

  it('renders one row per invocation with the result chip', () => {
    const { container } = render(<HookInvocationTimeline invocations={[invocation]} />);
    expect(container.querySelectorAll('.wb-event-row')).toHaveLength(1);
    expect(container.querySelector('.wb-badge')?.textContent).toContain('success');
  });

  it('expands the body when the row is clicked', () => {
    const { container } = render(<HookInvocationTimeline invocations={[invocation]} />);
    fireEvent.click(container.querySelector('.wb-event-toggle') as HTMLElement);
    expect(container.textContent).toContain('stdout');
    expect(container.textContent).toContain('stderr');
  });

  it('shows a no captured output hint when output is empty', () => {
    const { container } = render(
      <HookInvocationTimeline invocations={[{ ...invocation, stdout: '', stderr: '', error: '' }]} />,
    );
    fireEvent.click(container.querySelector('.wb-event-toggle') as HTMLElement);
    expect(container.textContent).toContain('No captured output.');
  });
});
