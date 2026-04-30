import { useState } from 'preact/hooks';
import type { AssistantBlock, ParsedMessage, UserContentItem } from '../lib/streamMessages';
import { stringifyContent } from '../lib/streamMessages';

const PREVIEW_LINES = 10;

function Collapsible({
  preview,
  full,
  language,
}: {
  preview: string;
  full: string;
  language?: string;
}) {
  const [open, setOpen] = useState(false);
  const truncated = preview !== full;
  return (
    <div class={`wb-sm-collapsible${language ? ` lang-${language}` : ''}`}>
      <pre>{open ? full : preview}</pre>
      {truncated && (
        <button type="button" class="wb-sm-toggle" onClick={() => setOpen((v) => !v)}>
          {open ? 'Collapse' : 'Expand'} ({full.split('\n').length} lines)
        </button>
      )}
    </div>
  );
}

function previewOf(text: string, lines = PREVIEW_LINES): string {
  const all = text.split('\n');
  if (all.length <= lines) return text;
  return all.slice(0, lines).join('\n') + `\n…(${all.length - lines} more lines)`;
}

function codeOrText(value: unknown): string {
  if (typeof value === 'string') return value;
  try {
    return JSON.stringify(value, null, 2);
  } catch {
    return String(value);
  }
}

function AssistantBlockView({ block }: { block: AssistantBlock }) {
  if (block.type === 'text') {
    return <div class="wb-sm-text">{block.text}</div>;
  }
  if (block.type === 'thinking') {
    return (
      <div class="wb-sm-thinking">
        <span class="wb-sm-tag">thinking</span>
        <Collapsible preview={previewOf(block.text)} full={block.text} />
      </div>
    );
  }
  if (block.type === 'tool_use') {
    const inputStr = codeOrText(block.input);
    return (
      <div class="wb-sm-tool-use" data-tool-id={block.id || ''}>
        <div class="wb-sm-tool-head">
          <span class="wb-sm-tag tool">tool_use</span>
          <strong>{block.name}</strong>
          {block.id && <code class="wb-sm-id">{block.id}</code>}
        </div>
        <Collapsible preview={previewOf(inputStr)} full={inputStr} language="json" />
      </div>
    );
  }
  if (block.type === 'image') {
    return (
      <div class="wb-sm-image">
        <span class="wb-sm-tag">image</span>
        <em>(image attachment)</em>
      </div>
    );
  }
  return (
    <div class="wb-sm-unknown">
      <span class="wb-sm-tag">unknown</span>
      <pre>{codeOrText(block.raw)}</pre>
    </div>
  );
}

function UserItemView({ item }: { item: UserContentItem }) {
  if (item.type === 'text') {
    return <div class="wb-sm-text">{item.text}</div>;
  }
  if (item.type === 'tool_result') {
    const text = stringifyContent(item.content);
    return (
      <div class={`wb-sm-tool-result${item.isError ? ' is-error' : ''}`}>
        <div class="wb-sm-tool-head">
          <span class="wb-sm-tag result">tool_result</span>
          {item.toolUseId && <code class="wb-sm-id">{item.toolUseId}</code>}
          {item.isError && <span class="wb-sm-err">ERROR</span>}
        </div>
        <Collapsible preview={previewOf(text)} full={text} />
      </div>
    );
  }
  if (item.type === 'image') {
    return (
      <div class="wb-sm-image">
        <span class="wb-sm-tag">image</span>
        <em>(image attachment)</em>
      </div>
    );
  }
  return (
    <div class="wb-sm-unknown">
      <pre>{codeOrText(item.raw)}</pre>
    </div>
  );
}

export function StreamMessageView({ msg }: { msg: ParsedMessage }) {
  if (msg.kind === 'unparsed') {
    return (
      <div class="wb-sm wb-sm-unparsed">
        <span class="wb-sm-tag warn">unparsed</span>
        <pre>{msg.line}</pre>
        {msg.error && <div class="wb-sm-err">{msg.error}</div>}
      </div>
    );
  }
  if (msg.kind === 'system') {
    return (
      <div class="wb-sm wb-sm-system" title={JSON.stringify(msg.raw)}>
        <span class="wb-sm-chip">system: {msg.summary}</span>
      </div>
    );
  }
  if (msg.kind === 'user') {
    return (
      <div class="wb-sm wb-sm-user">
        <div class="wb-sm-role">user</div>
        {msg.items.map((item, i) => (
          <UserItemView key={i} item={item} />
        ))}
      </div>
    );
  }
  if (msg.kind === 'assistant') {
    return (
      <div class="wb-sm wb-sm-assistant">
        <div class="wb-sm-role">assistant</div>
        {msg.blocks.map((block, i) => (
          <AssistantBlockView key={i} block={block} />
        ))}
      </div>
    );
  }
  if (msg.kind === 'tool_result') {
    return (
      <div class={`wb-sm wb-sm-tool-result${msg.isError ? ' is-error' : ''}`}>
        <div class="wb-sm-tool-head">
          <span class="wb-sm-tag result">tool_result</span>
          {msg.toolUseId && <code class="wb-sm-id">{msg.toolUseId}</code>}
          {msg.isError && <span class="wb-sm-err">ERROR</span>}
        </div>
        <Collapsible preview={previewOf(msg.content)} full={msg.content} />
      </div>
    );
  }
  if (msg.kind === 'result') {
    return (
      <div class="wb-sm wb-sm-result-card">
        <div class="wb-sm-role">{msg.summary}</div>
        {msg.usage && (
          <ul class="wb-sm-usage">
            {Object.entries(msg.usage).map(([k, v]) => (
              <li key={k}>
                <span>{k}</span>
                <strong>{v}</strong>
              </li>
            ))}
          </ul>
        )}
      </div>
    );
  }
  if (msg.kind === 'codex') {
    const paramsStr = codeOrText(msg.params);
    return (
      <div class="wb-sm wb-sm-codex">
        <div class="wb-sm-tool-head">
          <span class="wb-sm-tag codex">codex</span>
          <strong>{msg.method}</strong>
          {msg.itemType && <code class="wb-sm-id">{msg.itemType}</code>}
        </div>
        <div class="wb-sm-text">{msg.summary}</div>
        <Collapsible preview={previewOf(paramsStr)} full={paramsStr} language="json" />
      </div>
    );
  }
  // Exhaustive guard
  const _exhaustive: never = msg;
  return <pre>{JSON.stringify(_exhaustive)}</pre>;
}

export default StreamMessageView;
