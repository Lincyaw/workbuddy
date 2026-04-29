import { useRoute } from 'preact-iso';
import { Layout } from '../components/Layout';

export function SessionDetail() {
  const { params } = useRoute();
  return (
    <Layout>
      <h1>Session {params.id}</h1>
      <div class="empty">
        Session detail ships in a follow-up issue. Use{' '}
        <code>workbuddy logs {params.id}</code> in the meantime.
      </div>
    </Layout>
  );
}
