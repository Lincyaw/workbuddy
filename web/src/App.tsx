import { LocationProvider, Router, Route } from 'preact-iso';
import lazy, { ErrorBoundary } from 'preact-iso/lazy';
import { Dashboard } from './pages/Dashboard';
import { IssueDetail } from './pages/IssueDetail';
import { NotFound } from './pages/NotFound';

const Sessions = lazy(() => import('./pages/Sessions').then((m) => m.default));
const SessionDetail = lazy(() =>
  import('./pages/SessionDetail').then((m) => m.default),
);

export function App() {
  return (
    <LocationProvider>
      <ErrorBoundary>
        <Router>
          <Route path="/" component={Dashboard} />
          <Route path="/sessions" component={Sessions} />
          <Route path="/sessions/:id" component={SessionDetail} />
          <Route path="/issues/:owner/:repo/:num" component={IssueDetail} />
          <Route default component={NotFound} />
        </Router>
      </ErrorBoundary>
    </LocationProvider>
  );
}
