import { describe, expect, it } from 'vitest';
import type { SessionEvent } from '../api/sessions';
import { buildPrettyTimeline, parseStructuredEvent } from './sessionPretty';

function event(kind: string, payload: unknown, overrides: Partial<SessionEvent> = {}): SessionEvent {
  return {
    index: overrides.index ?? 0,
    kind,
    seq: overrides.seq ?? 1,
    turn_id: overrides.turn_id ?? 'turn-1',
    payload,
    ...overrides,
  };
}

describe('parseStructuredEvent', () => {
  it('maps codex agent.message final payloads to assistant text blocks', () => {
    const msg = parseStructuredEvent(event('agent.message', { text: 'done', final: true }), { runtime: 'codex' });
    expect(msg).toMatchObject({ kind: 'assistant', blocks: [{ type: 'text', text: 'done' }] });
  });

  it('maps reasoning payloads to thinking blocks', () => {
    const msg = parseStructuredEvent(event('reasoning', { summary: 'inspect repo' }), { runtime: 'codex' });
    expect(msg).toMatchObject({ kind: 'assistant', blocks: [{ type: 'thinking', text: 'inspect repo' }] });
  });

  it('maps command.exec payloads to bash tool_use cards', () => {
    const msg = parseStructuredEvent(
      event('command.exec', { cmd: ['bash', '-lc', 'pwd'], cwd: '/tmp', call_id: 'call-1' }),
      { runtime: 'codex' },
    );
    expect(msg).toMatchObject({
      kind: 'assistant',
      blocks: [
        {
          type: 'tool_use',
          id: 'call-1',
          name: 'bash',
          input: { command: 'bash -lc pwd', cwd: '/tmp' },
        },
      ],
    });
  });

  it('maps tool.result payloads to tool_result cards', () => {
    const msg = parseStructuredEvent(event('tool.result', { call_id: 'call-1', ok: false, error: 'boom' }), {
      runtime: 'codex',
    });
    expect(msg).toMatchObject({ kind: 'tool_result', toolUseId: 'call-1', content: 'boom', isError: true });
  });

  it('suppresses command.output in pretty mode', () => {
    const msg = parseStructuredEvent(event('command.output', { call_id: 'call-1', data: 'chunk' }), {
      runtime: 'codex',
    });
    expect(msg).toBeNull();
  });

  it('keeps claude log payload.line parsing unchanged', () => {
    const msg = parseStructuredEvent(
      event('log', {
        line: JSON.stringify({
          type: 'assistant',
          message: { role: 'assistant', content: [{ type: 'text', text: 'hello from claude' }] },
        }),
      }),
      { runtime: 'claude' },
    );
    expect(msg).toMatchObject({ kind: 'assistant', blocks: [{ type: 'text', text: 'hello from claude' }] });
  });
});

describe('buildPrettyTimeline', () => {
  it('collapses codex deltas in the same turn into one assistant paragraph and replaces with the final text', () => {
    const items = buildPrettyTimeline(
      [
        event('agent.message', { text: 'Hel', delta: true }, { index: 1 }),
        event('agent.message', { text: 'lo', delta: true }, { index: 2 }),
        event('agent.message', { text: 'Hello', final: true }, { index: 3 }),
      ],
      { runtime: 'codex' },
    );

    expect(items).toHaveLength(1);
    expect(items[0].msg).toMatchObject({ kind: 'assistant', blocks: [{ type: 'text', text: 'Hello' }] });
    expect(items[0].events).toHaveLength(3);
  });

  it('keeps tool_use, suppresses command.output, and leaves the result directly after it', () => {
    const items = buildPrettyTimeline(
      [
        event('command.exec', { cmd: 'echo hi', cwd: '/repo', call_id: 'call-1' }, { index: 1 }),
        event('command.output', { call_id: 'call-1', data: 'hi\n' }, { index: 2 }),
        event('tool.result', { call_id: 'call-1', ok: true, result: 'hi' }, { index: 3 }),
      ],
      { runtime: 'codex' },
    );

    expect(items).toHaveLength(2);
    expect(items[0].msg).toMatchObject({ kind: 'assistant' });
    expect(items[1].msg).toMatchObject({ kind: 'tool_result', content: 'hi' });
  });
});
