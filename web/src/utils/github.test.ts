import { test } from 'node:test';
import assert from 'node:assert/strict';
import { githubIssueURL, githubIssueURLFromSlug, splitRepoSlug } from './github.ts';

test('githubIssueURL builds canonical github issue URL', () => {
  assert.equal(
    githubIssueURL('Lincyaw', 'workbuddy', 250),
    'https://github.com/Lincyaw/workbuddy/issues/250',
  );
});

test('githubIssueURL accepts string num', () => {
  assert.equal(
    githubIssueURL('octocat', 'hello-world', '42'),
    'https://github.com/octocat/hello-world/issues/42',
  );
});

test('splitRepoSlug splits owner/name', () => {
  assert.deepEqual(splitRepoSlug('Lincyaw/workbuddy'), { owner: 'Lincyaw', name: 'workbuddy' });
});

test('splitRepoSlug handles missing slash', () => {
  assert.deepEqual(splitRepoSlug('orphan'), { owner: 'orphan', name: '' });
});

test('githubIssueURLFromSlug derives URL from owner/name slug', () => {
  assert.equal(
    githubIssueURLFromSlug('Lincyaw/workbuddy', 250),
    'https://github.com/Lincyaw/workbuddy/issues/250',
  );
});
