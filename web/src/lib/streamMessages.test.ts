import { describe, expect, it } from 'vitest';
import { pairToolResults, parseLine } from './streamMessages';

describe('parseLine', () => {
  it('parses a system hook line', () => {
    const line = JSON.stringify({
      type: 'system',
      subtype: 'hook_started',
      hook_name: 'SessionStart:startup',
      session_id: 'abc',
      uuid: 'u-1',
    });
    const m = parseLine(line);
    expect(m.kind).toBe('system');
    if (m.kind !== 'system') throw new Error('unreachable');
    expect(m.subtype).toBe('hook_started');
    expect(m.hookName).toBe('SessionStart:startup');
    expect(m.summary).toBe('hook_started · SessionStart:startup');
  });

  it('parses an assistant message with text and tool_use blocks', () => {
    const line = JSON.stringify({
      type: 'assistant',
      message: {
        role: 'assistant',
        content: [
          { type: 'text', text: 'Reading the issue...' },
          {
            type: 'tool_use',
            id: 'toolu_1',
            name: 'Bash',
            input: { command: 'gh issue view 250' },
          },
        ],
      },
    });
    const m = parseLine(line);
    expect(m.kind).toBe('assistant');
    if (m.kind !== 'assistant') throw new Error('unreachable');
    expect(m.blocks).toHaveLength(2);
    expect(m.blocks[0]).toEqual({ type: 'text', text: 'Reading the issue...' });
    expect(m.blocks[1]).toMatchObject({
      type: 'tool_use',
      id: 'toolu_1',
      name: 'Bash',
    });
  });

  it('parses a top-level tool_result line', () => {
    const line = JSON.stringify({
      type: 'tool_result',
      tool_use_id: 'toolu_1',
      content: 'On branch main',
      is_error: false,
    });
    const m = parseLine(line);
    expect(m.kind).toBe('tool_result');
    if (m.kind !== 'tool_result') throw new Error('unreachable');
    expect(m.toolUseId).toBe('toolu_1');
    expect(m.content).toBe('On branch main');
    expect(m.isError).toBe(false);
  });

  it('parses a user message containing a tool_result block', () => {
    const line = JSON.stringify({
      type: 'user',
      message: {
        role: 'user',
        content: [
          {
            type: 'tool_result',
            tool_use_id: 'toolu_1',
            content: [{ type: 'text', text: 'output here' }],
            is_error: false,
          },
        ],
      },
    });
    const m = parseLine(line);
    expect(m.kind).toBe('user');
    if (m.kind !== 'user') throw new Error('unreachable');
    expect(m.items).toHaveLength(1);
    expect(m.items[0]).toMatchObject({
      type: 'tool_result',
      toolUseId: 'toolu_1',
      isError: false,
    });
  });

  it('parses a result summary line with usage', () => {
    const line = JSON.stringify({
      type: 'result',
      subtype: 'success',
      usage: { input_tokens: 100, output_tokens: 42 },
      total_cost_usd: 0.0123,
    });
    const m = parseLine(line);
    expect(m.kind).toBe('result');
    if (m.kind !== 'result') throw new Error('unreachable');
    expect(m.summary).toBe('result · success');
    expect(m.usage).toMatchObject({ input_tokens: 100, output_tokens: 42, total_cost_usd: 0.0123 });
  });

  it('parses a codex app-server method line', () => {
    const line = JSON.stringify({
      method: 'item/agentMessage/delta',
      params: { threadId: 't1', turnId: 'turn-1', itemId: 'i-1', delta: 'PONG' },
    });
    const m = parseLine(line, { runtime: 'codex' });
    expect(m.kind).toBe('codex');
    if (m.kind !== 'codex') throw new Error('unreachable');
    expect(m.method).toBe('item/agentMessage/delta');
    expect(m.summary).toContain('PONG');
  });

  it('falls back to unparsed for malformed JSON', () => {
    const m = parseLine('not-json{');
    expect(m.kind).toBe('unparsed');
    if (m.kind !== 'unparsed') throw new Error('unreachable');
    expect(m.line).toBe('not-json{');
    expect(m.error).toBeTruthy();
  });

  it('marks unparsed when type field is absent', () => {
    const m = parseLine(JSON.stringify({ foo: 'bar' }));
    expect(m.kind).toBe('unparsed');
  });
});

describe('pairToolResults', () => {
  it('indexes by tool_use_id from both top-level and user-block forms', () => {
    const messages = [
      parseLine(JSON.stringify({ type: 'tool_result', tool_use_id: 'a', content: 'x' })),
      parseLine(
        JSON.stringify({
          type: 'user',
          message: {
            role: 'user',
            content: [{ type: 'tool_result', tool_use_id: 'b', content: 'y' }],
          },
        }),
      ),
    ];
    const map = pairToolResults(messages);
    expect(map.get('a')).toBe(0);
    expect(map.get('b')).toBe(1);
  });
});
