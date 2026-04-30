import { githubIssueURL } from '../utils/github';

interface Props {
  owner: string;
  repo: string;
  num: number | string;
  variant?: 'text' | 'icon';
  label?: string;
}

export function GitHubIssueLink({ owner, repo, num, variant = 'text', label }: Props) {
  const href = githubIssueURL(owner, repo, num);
  const text = label ?? (variant === 'icon' ? '↗' : `Open #${num} on GitHub ↗`);
  const title = `Open ${owner}/${repo}#${num} on GitHub`;
  return (
    <a
      href={href}
      target="_blank"
      rel="noopener noreferrer"
      class={variant === 'icon' ? 'gh-issue-link gh-issue-link-icon' : 'gh-issue-link'}
      title={title}
      aria-label={title}
      data-testid="github-issue-link"
    >
      {text}
    </a>
  );
}
