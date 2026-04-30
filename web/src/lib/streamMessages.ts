/**
 * Parser for stream-json lines emitted by Claude Code and the Codex
 * app-server. The session detail page receives these as `kind=log`
 * events with `payload.line` set to a single JSON line. The parser
 * normalises both runtimes onto a small discriminated union so the
 * UI can render them structurally.
 */

export type AssistantBlock =
  | { type: 'text'; text: string }
  | { type: 'thinking'; text: string }
  | { type: 'tool_use'; id?: string; name: string; input: unknown }
  | { type: 'image'; source?: unknown }
  | { type: 'unknown'; raw: unknown };

export type UserContentItem =
  | { type: 'text'; text: string }
  | { type: 'tool_result'; toolUseId?: string; content: unknown; isError: boolean }
  | { type: 'image'; source?: unknown }
  | { type: 'unknown'; raw: unknown };

export type ParsedMessage =
  | {
      kind: 'system';
      subtype?: string;
      hookName?: string;
      sessionId?: string;
      summary: string;
      raw: unknown;
    }
  | {
      kind: 'user';
      text: string;
      items: UserContentItem[];
      raw: unknown;
    }
  | {
      kind: 'assistant';
      blocks: AssistantBlock[];
      raw: unknown;
    }
  | {
      kind: 'tool_result';
      toolUseId?: string;
      content: string;
      isError: boolean;
      raw: unknown;
    }
  | {
      kind: 'result';
      summary: string;
      usage?: Record<string, number>;
      raw: unknown;
    }
  | {
      kind: 'codex';
      method: string;
      itemType?: string;
      summary: string;
      params: unknown;
      raw: unknown;
    }
  | { kind: 'unparsed'; line: string; error?: string };

export type Runtime = 'claude' | 'codex' | 'unknown';

export interface ParseOptions {
  runtime?: Runtime;
}

function isRecord(v: unknown): v is Record<string, unknown> {
  return typeof v === 'object' && v !== null && !Array.isArray(v);
}

function asString(v: unknown): string | undefined {
  return typeof v === 'string' ? v : undefined;
}

export function parseLine(line: string, opts: ParseOptions = {}): ParsedMessage {
  const trimmed = (line ?? '').trim();
  if (!trimmed) return { kind: 'unparsed', line, error: 'empty line' };
  let raw: unknown;
  try {
    raw = JSON.parse(trimmed);
  } catch (err) {
    return { kind: 'unparsed', line, error: (err as Error).message };
  }
  if (!isRecord(raw)) {
    return { kind: 'unparsed', line, error: 'not an object' };
  }
  // Codex app-server uses JSON-RPC style with a top-level "method" field
  // (e.g. "turn/started", "item/agentMessage/delta"). Detect explicitly so
  // we don't accidentally route Claude messages through the codex branch.
  if (typeof raw.method === 'string' && (opts.runtime === 'codex' || !('type' in raw))) {
    return parseCodex(raw, raw.method);
  }
  const t = asString(raw.type);
  switch (t) {
    case 'system':
      return parseSystem(raw);
    case 'user':
      return parseUser(raw);
    case 'assistant':
      return parseAssistant(raw);
    case 'tool_result':
    case 'user.tool_result':
      return parseToolResult(raw);
    case 'result':
      return parseResult(raw);
    default:
      // Codex sometimes wraps payload as {method,params} but other times
      // nests under {jsonrpc, result}. Try a fallback when runtime hints codex.
      if (opts.runtime === 'codex' && typeof raw.method === 'string') {
        return parseCodex(raw, raw.method);
      }
      return {
        kind: 'unparsed',
        line,
        error: t ? `unsupported type: ${t}` : 'no type field',
      };
  }
}

function parseSystem(raw: Record<string, unknown>): ParsedMessage {
  const subtype = asString(raw.subtype);
  const hookName = asString(raw.hook_name);
  const sessionId = asString(raw.session_id) ?? asString(raw.sessionId);
  const parts: string[] = [];
  if (subtype) parts.push(subtype);
  if (hookName) parts.push(hookName);
  if (parts.length === 0) parts.push('event');
  return {
    kind: 'system',
    subtype,
    hookName,
    sessionId,
    summary: parts.join(' · '),
    raw,
  };
}

function parseAssistant(raw: Record<string, unknown>): ParsedMessage {
  const message = isRecord(raw.message) ? raw.message : raw;
  const content = (message as Record<string, unknown>).content;
  const blocks: AssistantBlock[] = [];
  if (Array.isArray(content)) {
    for (const block of content) {
      blocks.push(coerceAssistantBlock(block));
    }
  } else if (typeof content === 'string') {
    blocks.push({ type: 'text', text: content });
  }
  return { kind: 'assistant', blocks, raw };
}

function coerceAssistantBlock(block: unknown): AssistantBlock {
  if (!isRecord(block)) return { type: 'unknown', raw: block };
  const t = asString(block.type);
  switch (t) {
    case 'text':
      return { type: 'text', text: asString(block.text) ?? '' };
    case 'thinking':
      return { type: 'thinking', text: asString(block.thinking) ?? asString(block.text) ?? '' };
    case 'tool_use':
      return {
        type: 'tool_use',
        id: asString(block.id),
        name: asString(block.name) ?? 'tool',
        input: block.input ?? {},
      };
    case 'image':
      return { type: 'image', source: block.source };
    default:
      return { type: 'unknown', raw: block };
  }
}

function parseUser(raw: Record<string, unknown>): ParsedMessage {
  const message = isRecord(raw.message) ? raw.message : raw;
  const content = (message as Record<string, unknown>).content;
  const items: UserContentItem[] = [];
  let text = '';
  if (typeof content === 'string') {
    text = content;
    items.push({ type: 'text', text: content });
  } else if (Array.isArray(content)) {
    for (const block of content) {
      if (!isRecord(block)) {
        items.push({ type: 'unknown', raw: block });
        continue;
      }
      const t = asString(block.type);
      if (t === 'text') {
        const s = asString(block.text) ?? '';
        text += (text ? '\n' : '') + s;
        items.push({ type: 'text', text: s });
      } else if (t === 'tool_result') {
        items.push({
          type: 'tool_result',
          toolUseId: asString(block.tool_use_id),
          content: block.content,
          isError: Boolean(block.is_error),
        });
      } else if (t === 'image') {
        items.push({ type: 'image', source: block.source });
      } else {
        items.push({ type: 'unknown', raw: block });
      }
    }
  }
  return { kind: 'user', text, items, raw };
}

function parseToolResult(raw: Record<string, unknown>): ParsedMessage {
  return {
    kind: 'tool_result',
    toolUseId: asString(raw.tool_use_id) ?? asString(raw.toolUseId),
    content: stringifyContent(raw.content),
    isError: Boolean(raw.is_error),
    raw,
  };
}

export function stringifyContent(content: unknown): string {
  if (content == null) return '';
  if (typeof content === 'string') return content;
  if (Array.isArray(content)) {
    return content
      .map((item) => {
        if (typeof item === 'string') return item;
        if (isRecord(item) && asString(item.type) === 'text') {
          return asString(item.text) ?? '';
        }
        try {
          return JSON.stringify(item, null, 2);
        } catch {
          return String(item);
        }
      })
      .join('\n');
  }
  try {
    return JSON.stringify(content, null, 2);
  } catch {
    return String(content);
  }
}

function parseResult(raw: Record<string, unknown>): ParsedMessage {
  const usage: Record<string, number> = {};
  const u = raw.usage;
  if (isRecord(u)) {
    for (const [k, v] of Object.entries(u)) {
      if (typeof v === 'number') usage[k] = v;
    }
  }
  // Top-level numeric counters that some runtimes emit alongside usage.
  for (const k of ['input_tokens', 'output_tokens', 'total_cost_usd', 'duration_ms']) {
    const v = raw[k];
    if (typeof v === 'number') usage[k] = v;
  }
  const subtype = asString(raw.subtype);
  const summary = subtype ? `result · ${subtype}` : 'result';
  return {
    kind: 'result',
    summary,
    usage: Object.keys(usage).length > 0 ? usage : undefined,
    raw,
  };
}

function parseCodex(raw: Record<string, unknown>, method: string): ParsedMessage {
  const params = isRecord(raw.params) ? raw.params : {};
  let itemType: string | undefined;
  let summary = method;
  if (method === 'item/started' || method === 'item/completed' || method === 'item/updated') {
    const item = isRecord((params as Record<string, unknown>).item)
      ? ((params as Record<string, unknown>).item as Record<string, unknown>)
      : undefined;
    itemType = asString(item?.type);
    if (itemType) summary = `${method} · ${itemType}`;
    if (itemType === 'agentMessage' && item) {
      const text = asString(item.text);
      if (text) summary = `${method} · agentMessage · ${truncate(text, 60)}`;
    }
  } else if (method === 'item/agentMessage/delta') {
    summary = `delta · ${truncate(asString((params as Record<string, unknown>).delta) ?? '', 60)}`;
  } else if (method === 'turn/started' || method === 'turn/completed') {
    const turn = isRecord((params as Record<string, unknown>).turn)
      ? ((params as Record<string, unknown>).turn as Record<string, unknown>)
      : undefined;
    const status = asString(turn?.status);
    if (status) summary = `${method} · ${status}`;
  }
  return { kind: 'codex', method, itemType, summary, params, raw };
}

function truncate(s: string, n: number): string {
  const flat = s.replace(/\s+/g, ' ').trim();
  return flat.length > n ? flat.slice(0, n) + '…' : flat;
}

/**
 * Group a sequence of parsed messages by turn boundaries. A new turn
 * starts whenever a `system` `init`-like event or a Codex `turn/started`
 * is encountered, or whenever the upstream `turn_id` changes.
 */
export interface TurnGroup<T> {
  turnId: string;
  items: T[];
}

export function groupByTurn<T extends { turnId?: string; index: number }>(items: T[]): TurnGroup<T>[] {
  const out: TurnGroup<T>[] = [];
  let current: TurnGroup<T> | null = null;
  for (const item of items) {
    const tid = item.turnId || '';
    if (!current || current.turnId !== tid) {
      current = { turnId: tid, items: [] };
      out.push(current);
    }
    current.items.push(item);
  }
  return out;
}

/**
 * Walk a list of parsed log messages and pair each `tool_result` with its
 * preceding `tool_use` block (Claude only). Returns the index map so the
 * UI can render a result inline beneath its call.
 */
export function pairToolResults(messages: ParsedMessage[]): Map<string, number> {
  const map = new Map<string, number>();
  messages.forEach((msg, idx) => {
    if (msg.kind === 'tool_result' && msg.toolUseId) {
      map.set(msg.toolUseId, idx);
    } else if (msg.kind === 'user') {
      for (const item of msg.items) {
        if (item.type === 'tool_result' && item.toolUseId) {
          map.set(item.toolUseId, idx);
        }
      }
    }
  });
  return map;
}
