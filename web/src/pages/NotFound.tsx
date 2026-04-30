import { Layout } from '../components/Layout';
import { EmptyState } from '../components/EmptyState';

export function NotFound() {
  return (
    <Layout>
      <div class="wb-page-header wb-page-header--tight">
        <div>
          <p class="wb-eyebrow">Navigation</p>
          <h1 class="wb-page-title">Not found</h1>
          <p class="wb-page-subtitle">That route does not match a Workbuddy page in this build.</p>
        </div>
      </div>
      <EmptyState
        icon="?"
        title="We couldn't find that screen"
        copy={<>Head back to the <a href="/">dashboard</a> or use the topbar to jump to sessions and hooks.</>}
      />
    </Layout>
  );
}
