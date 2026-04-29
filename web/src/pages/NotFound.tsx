import { Layout } from '../components/Layout';

export function NotFound() {
  return (
    <Layout>
      <h1>Not found</h1>
      <p class="muted">
        That route doesn't match any page. Head back to the{' '}
        <a href="/">dashboard</a>.
      </p>
    </Layout>
  );
}
