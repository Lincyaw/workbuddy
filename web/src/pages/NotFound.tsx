import { Layout } from '../components/Layout';
import { EmptyState } from '../components/EmptyState';

export function NotFound() {
  return (
    <Layout>
      <section class="surface-card">
        <EmptyState
          title="this route doesn't exist"
          detail="you might be looking for /sessions or /dashboard."
          ctaHref="/dashboard"
          ctaLabel="return to dashboard →"
        />
      </section>
    </Layout>
  );
}
