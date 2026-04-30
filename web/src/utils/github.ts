export function githubIssueURL(owner: string, repo: string, num: number | string): string {
  return `https://github.com/${owner}/${repo}/issues/${num}`;
}

export function splitRepoSlug(repo: string): { owner: string; name: string } {
  const slash = repo.indexOf('/');
  if (slash <= 0) return { owner: repo, name: '' };
  return { owner: repo.slice(0, slash), name: repo.slice(slash + 1) };
}

export function githubIssueURLFromSlug(repo: string, num: number | string): string {
  const { owner, name } = splitRepoSlug(repo);
  return githubIssueURL(owner, name, num);
}
