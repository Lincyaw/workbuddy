import { render, waitFor } from '@testing-library/preact';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { SessionDetail } from './SessionDetail';
import { fetchSession, fetchSessionEvents } from '../api/sessions';

vi.mock('preact-iso', () => ({
  useRoute: () => ({ params: { id: 'session-1' } }),
}));

vi.mock('../components/Layout', () => ({
  Layout: ({ children }: { children: any }) => <div data-testid="layout">{children}</div>,
}));

vi.mock('../components/DegradedSessionCard', () => ({
  DegradedSessionCard: () => null,
}));

vi.mock('../components/EmptyState', () => ({
  EmptyState: ({ title }: { title: string }) => <div>{title}</div>,
}));

vi.mock('../components/GitHubIssueLink', () => ({
  GitHubIssueLink: () => <span>gh</span>,
}));

vi.mock('../api/sessions', () => ({
  fetchSession: vi.fn(),
  fetchSessionEvents: vi.fn(),
  sessionStreamURL: vi.fn(() => '/api/v1/sessions/session-1/stream?after=0'),
}));

class FakeEventSource {
  onopen: (() => void) | null = null;
  onerror: (() => void) | null = null;

  addEventListener() {}
  close() {}
}

function meta(runtime: 'claude' | 'codex') {
  return {
    session_id: 'session-1',
    repo: 'Lincyaw/workbuddy',
    issue_num: 295,
    agent_name: 'dev-agent',
    runtime,
    attempt: 1,
    status: 'running',
    exit_code: 0,
    duration: 12,
    created_at: '2026-04-30T12:00:00Z',
    artifact_paths: {},
  };
}

beforeEach(() => {
  vi.clearAllMocks();
  vi.stubGlobal('EventSource', FakeEventSource as unknown as typeof EventSource);
});

afterEach(() => {
  vi.unstubAllGlobals();
});

describe('SessionDetail pretty mode', () => {
  it('renders codex command/tool/text cards from structured events', async () => {
    vi.mocked(fetchSession).mockResolvedValue(meta('codex') as never);
    vi.mocked(fetchSessionEvents).mockResolvedValue(
      {
        total: 5,
        events: [
          { index: 0, kind: 'turn.started', seq: 1, turn_id: 'turn-1', payload: { summary: 'turn start' } },
          {
            index: 1,
            kind: 'command.exec',
            seq: 2,
            turn_id: 'turn-1',
            payload: { cmd: ['bash', '-lc', 'pwd'], cwd: '/repo', call_id: 'call-1' },
          },
          {
            index: 2,
            kind: 'command.output',
            seq: 3,
            turn_id: 'turn-1',
            payload: { call_id: 'call-1', stream: 'stdout', data: '/repo\n' },
          },
          {
            index: 3,
            kind: 'tool.result',
            seq: 4,
            turn_id: 'turn-1',
            payload: { call_id: 'call-1', ok: true, result: '/repo' },
          },
          {
            index: 4,
            kind: 'agent.message',
            seq: 5,
            turn_id: 'turn-1',
            payload: { text: 'Done.', final: true },
          },
        ],
      } as never,
    );

    const { container } = render(<SessionDetail />);
    await waitFor(() => expect(container.querySelector('.wb-sm-tool-use')).not.toBeNull());
    expect(container.querySelector('.wb-sm-tool-result')).not.toBeNull();
    expect(container.querySelector('.wb-sm-text')?.textContent).toContain('Done.');
    expect(container.textContent).not.toContain('/repo\n');
  });

  it('keeps claude log pretty rendering working through payload.line parsing', async () => {
    vi.mocked(fetchSession).mockResolvedValue(meta('claude') as never);
    vi.mocked(fetchSessionEvents).mockResolvedValue(
      {
        total: 3,
        events: [
          {
            index: 0,
            kind: 'log',
            seq: 1,
            turn_id: 'turn-1',
            payload: {
              line: JSON.stringify({
                type: 'assistant',
                message: { role: 'assistant', content: [{ type: 'text', text: 'Reading issue' }] },
              }),
            },
          },
          {
            index: 1,
            kind: 'log',
            seq: 2,
            turn_id: 'turn-1',
            payload: {
              line: JSON.stringify({
                type: 'assistant',
                message: {
                  role: 'assistant',
                  content: [{ type: 'tool_use', id: 'toolu_1', name: 'Bash', input: { command: 'pwd' } }],
                },
              }),
            },
          },
          {
            index: 2,
            kind: 'log',
            seq: 3,
            turn_id: 'turn-1',
            payload: {
              line: JSON.stringify({ type: 'tool_result', tool_use_id: 'toolu_1', content: '/repo', is_error: false }),
            },
          },
        ],
      } as never,
    );

    const { container } = render(<SessionDetail />);
    await waitFor(() => expect(container.querySelector('.wb-sm-tool-use')).not.toBeNull());
    expect(container.querySelector('.wb-sm-assistant')).not.toBeNull();
    expect(container.querySelector('.wb-sm-tool-result')).not.toBeNull();
    expect(container.textContent).toContain('Reading issue');
  });
});
