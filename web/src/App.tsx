import { LocationProvider, Router, Route } from 'preact-iso';
import lazy, { ErrorBoundary } from 'preact-iso/lazy';
import { Dashboard } from './pages/Dashboard';
import { IssueDetail } from './pages/IssueDetail';
import { NotFound } from './pages/NotFound';

const Sessions = lazy(() => import('./pages/Sessions').then((m) => m.default));
const SessionDetail = lazy(() =>
  import('./pages/SessionDetail').then((m) => m.default),
);
const Hooks = lazy(() => import('./pages/Hooks').then((m) => m.default));
const HookDetail = lazy(() =>
  import('./pages/HookDetail').then((m) => m.default),
);
const RolloutCompare = lazy(() =>
  import('./pages/RolloutCompare').then((m) => m.default),
);

export function App() {
  return (
    <LocationProvider>
      <ErrorBoundary>
        <Router>
          <Route path="/" component={Dashboard} />
          <Route path="/dashboard" component={Dashboard} />
          <Route path="/sessions" component={Sessions} />
          <Route path="/sessions/:id" component={SessionDetail} />
          <Route path="/issues/:owner/:repo/:num/rollouts/compare" component={RolloutCompare} />
          <Route path="/issues/:owner/:repo/:num" component={IssueDetail} />
          <Route path="/hooks" component={Hooks} />
          <Route path="/hooks/:name" component={HookDetail} />
          <Route default component={NotFound} />
        </Router>
      </ErrorBoundary>
    </LocationProvider>
  );
}
