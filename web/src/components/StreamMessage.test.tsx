import { render } from '@testing-library/preact';
import { describe, expect, it } from 'vitest';
import { parseLine } from '../lib/streamMessages';
import { StreamMessageView } from './StreamMessage';

function renderLine(line: string, runtime?: 'claude' | 'codex') {
  const msg = parseLine(line, runtime ? { runtime } : {});
  return render(<StreamMessageView msg={msg} />);
}

describe('StreamMessageView', () => {
  it('renders a system hook chip', () => {
    const { container } = renderLine(
      JSON.stringify({ type: 'system', subtype: 'hook_started', hook_name: 'SessionStart:startup' }),
    );
    expect(container.querySelector('.wb-sm-chip')?.textContent).toContain('hook_started');
    expect(container.querySelector('.wb-sm-chip')?.textContent).toContain('SessionStart:startup');
  });

  it('renders assistant text and tool_use blocks', () => {
    const { container } = renderLine(
      JSON.stringify({
        type: 'assistant',
        message: {
          role: 'assistant',
          content: [
            { type: 'text', text: 'Hello world' },
            { type: 'tool_use', id: 't1', name: 'Bash', input: { command: 'ls' } },
          ],
        },
      }),
    );
    expect(container.textContent).toContain('Hello world');
    expect(container.querySelector('.wb-sm-tool-use')).toBeTruthy();
    expect(container.querySelector('.wb-sm-tool-use')?.textContent).toContain('Bash');
  });

  it('renders tool_result with collapsed preview when content is long', () => {
    const longText = Array.from({ length: 50 }, (_, i) => `line ${i}`).join('\n');
    const { container } = renderLine(
      JSON.stringify({ type: 'tool_result', tool_use_id: 't1', content: longText, is_error: false }),
    );
    expect(container.querySelector('.wb-sm-toggle')?.textContent).toContain('Expand');
    expect(container.querySelector('pre')?.textContent).toContain('more lines');
  });

  it('renders an error tool_result with ERROR badge', () => {
    const { container } = renderLine(
      JSON.stringify({ type: 'tool_result', tool_use_id: 't2', content: 'boom', is_error: true }),
    );
    expect(container.querySelector('.wb-sm-err')?.textContent).toBe('ERROR');
  });

  it('renders unparsed lines with the original string', () => {
    const { container } = render(
      <StreamMessageView msg={{ kind: 'unparsed', line: 'oops not json' }} />,
    );
    expect(container.querySelector('.wb-sm-tag')?.textContent?.toLowerCase()).toContain('unparsed');
    expect(container.textContent).toContain('oops not json');
  });

  it('renders a result card with usage table', () => {
    const { container } = renderLine(
      JSON.stringify({ type: 'result', subtype: 'success', usage: { input_tokens: 10, output_tokens: 5 } }),
    );
    expect(container.querySelector('.wb-sm-result-card')).toBeTruthy();
    expect(container.textContent).toContain('input_tokens');
    expect(container.textContent).toContain('10');
  });

  it('renders codex methods with summary', () => {
    const { container } = renderLine(
      JSON.stringify({ method: 'turn/started', params: { turn: { status: 'inProgress' } } }),
      'codex',
    );
    expect(container.querySelector('.wb-sm-codex')).toBeTruthy();
    expect(container.textContent).toContain('turn/started');
    expect(container.textContent).toContain('inProgress');
  });
});
