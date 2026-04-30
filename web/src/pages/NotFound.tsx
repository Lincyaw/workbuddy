import { EmptyState } from '../components/EmptyState';
import { Layout } from '../components/Layout';

export function NotFound() {
  return (
    <Layout>
      <section class="wb-stack">
        <header>
          <p class="wb-section-label">navigation</p>
          <h1 class="wb-page-title">404</h1>
        </header>
        <EmptyState
          glyph="missing"
          title="this route doesn't exist"
          copy="you might be looking for /sessions or /dashboard."
          cta={<a href="/" class="wb-cta wb-cta--primary">return to dashboard</a>}
        />
      </section>
    </Layout>
  );
}
