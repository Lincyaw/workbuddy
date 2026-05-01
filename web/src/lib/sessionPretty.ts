import type { SessionEvent } from '../api/sessions';
import type { ParsedMessage, Runtime } from './streamMessages';
import { parseLine, stringifyContent } from './streamMessages';

interface BuildPrettyTimelineOptions {
  runtime?: Runtime;
  summarizeEvent?: (event: SessionEvent) => string;
}

export interface PrettyTimelineItem {
  key: string;
  turnId: string;
  kind: string;
  events: SessionEvent[];
  msg: ParsedMessage;
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value);
}

function asString(value: unknown): string | undefined {
  return typeof value === 'string' ? value : undefined;
}

function commandText(value: unknown): string {
  if (typeof value === 'string') return value;
  if (Array.isArray(value)) {
    return value
      .map((part) => {
        if (typeof part !== 'string') return String(part);
        return /[\s"'\\]/.test(part) ? JSON.stringify(part) : part;
      })
      .join(' ');
  }
  return stringifyContent(value);
}

function defaultSummary(event: SessionEvent): string {
  return event.kind || 'event';
}

export function parseStructuredEvent(
  event: SessionEvent,
  opts: BuildPrettyTimelineOptions = {},
): ParsedMessage | null {
  const payload = isRecord(event.payload) ? event.payload : {};
  const summary = (opts.summarizeEvent || defaultSummary)(event);

  if (event.kind === 'log') {
    const line = asString(payload.line);
    if (line) return parseLine(line, { runtime: opts.runtime });
    return { kind: 'system', summary, raw: payload };
  }

  if (event.kind === 'agent.message') {
    const text = asString(payload.text) ?? '';
    return {
      kind: 'assistant',
      blocks: [{ type: 'text', text }],
      raw: payload,
    };
  }

  if (event.kind === 'reasoning') {
    const text = asString(payload.text) ?? asString(payload.summary) ?? '';
    return {
      kind: 'assistant',
      blocks: [{ type: 'thinking', text }],
      raw: payload,
    };
  }

  if (event.kind === 'command.exec') {
    return {
      kind: 'assistant',
      blocks: [
        {
          type: 'tool_use',
          id: asString(payload.call_id),
          name: 'bash',
          input: {
            command: commandText(payload.cmd),
            cwd: asString(payload.cwd) ?? '',
          },
        },
      ],
      raw: payload,
    };
  }

  if (event.kind === 'tool.result') {
    const content = payload.result ?? payload.error;
    return {
      kind: 'tool_result',
      toolUseId: asString(payload.call_id),
      content: stringifyContent(content),
      isError: payload.ok === false,
      raw: payload,
    };
  }

  if (event.kind === 'command.output') {
    return null;
  }

  if (
    event.kind === 'turn.started' ||
    event.kind === 'turn.completed' ||
    event.kind === 'token.usage' ||
    event.kind === 'permission' ||
    event.kind === 'system'
  ) {
    return { kind: 'system', summary, raw: payload };
  }

  if (opts.runtime === 'codex') {
    return { kind: 'system', summary, raw: payload };
  }

  return null;
}

function isDeltaEvent(event: SessionEvent, kind: 'agent.message' | 'reasoning'): boolean {
  if (event.kind !== kind) return false;
  const payload = isRecord(event.payload) ? event.payload : {};
  return payload.delta === true;
}

function isFinalAgentMessage(event: SessionEvent): boolean {
  if (event.kind !== 'agent.message') return false;
  const payload = isRecord(event.payload) ? event.payload : {};
  return payload.final === true;
}

function isFinalReasoning(event: SessionEvent): boolean {
  if (event.kind !== 'reasoning') return false;
  const payload = isRecord(event.payload) ? event.payload : {};
  return payload.delta !== true;
}

function assistantText(item: PrettyTimelineItem): string | null {
  if (item.msg.kind !== 'assistant' || item.msg.blocks.length !== 1) return null;
  const block = item.msg.blocks[0];
  return block.type === 'text' ? block.text : null;
}

function assistantThinking(item: PrettyTimelineItem): string | null {
  if (item.msg.kind !== 'assistant' || item.msg.blocks.length !== 1) return null;
  const block = item.msg.blocks[0];
  return block.type === 'thinking' ? block.text : null;
}

export function buildPrettyTimeline(
  events: SessionEvent[],
  opts: BuildPrettyTimelineOptions = {},
): PrettyTimelineItem[] {
  const out: PrettyTimelineItem[] = [];

  for (const event of events) {
    const msg = parseStructuredEvent(event, opts);
    if (!msg) continue;

    const turnId = event.turn_id || '';
    const previous = out[out.length - 1];

    if (
      msg.kind === 'assistant' &&
      event.kind === 'agent.message' &&
      previous &&
      previous.kind === 'agent.message' &&
      previous.turnId === turnId
    ) {
      const nextText = assistantText({ key: '', turnId, kind: event.kind, events: [event], msg });
      const previousText = assistantText(previous);
      if (nextText != null && previousText != null) {
        if (isDeltaEvent(event, 'agent.message') && isDeltaEvent(previous.events[previous.events.length - 1], 'agent.message')) {
          previous.events.push(event);
          previous.msg = {
            kind: 'assistant',
            blocks: [{ type: 'text', text: previousText + nextText }],
            raw: previous.events[0].payload,
          };
          continue;
        }
        if (isFinalAgentMessage(event) && isDeltaEvent(previous.events[previous.events.length - 1], 'agent.message')) {
          previous.events.push(event);
          previous.msg = {
            kind: 'assistant',
            blocks: [{ type: 'text', text: nextText }],
            raw: previous.events[0].payload,
          };
          continue;
        }
      }
    }

    if (
      msg.kind === 'assistant' &&
      event.kind === 'reasoning' &&
      previous &&
      previous.kind === 'reasoning' &&
      previous.turnId === turnId
    ) {
      const nextText = assistantThinking({ key: '', turnId, kind: event.kind, events: [event], msg });
      const previousText = assistantThinking(previous);
      if (nextText != null && previousText != null) {
        if (isDeltaEvent(event, 'reasoning') && isDeltaEvent(previous.events[previous.events.length - 1], 'reasoning')) {
          previous.events.push(event);
          previous.msg = {
            kind: 'assistant',
            blocks: [{ type: 'thinking', text: previousText + nextText }],
            raw: previous.events[0].payload,
          };
          continue;
        }
        if (isFinalReasoning(event) && isDeltaEvent(previous.events[previous.events.length - 1], 'reasoning')) {
          previous.events.push(event);
          previous.msg = {
            kind: 'assistant',
            blocks: [{ type: 'thinking', text: nextText }],
            raw: previous.events[0].payload,
          };
          continue;
        }
      }
    }

    out.push({
      key: `${event.index}:${event.kind}:${out.length}`,
      turnId,
      kind: event.kind,
      events: [event],
      msg,
    });
  }

  return out;
}
