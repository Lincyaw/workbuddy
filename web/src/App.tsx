import { LocationProvider, Router, Route } from 'preact-iso';
import { Dashboard } from './pages/Dashboard';
import { IssueDetail } from './pages/IssueDetail';
import { Sessions } from './pages/Sessions';
import { SessionDetail } from './pages/SessionDetail';
import { NotFound } from './pages/NotFound';

export function App() {
  return (
    <LocationProvider>
      <Router>
        <Route path="/" component={Dashboard} />
        <Route path="/sessions" component={Sessions} />
        <Route path="/sessions/:id" component={SessionDetail} />
        <Route path="/issues/:owner/:repo/:num" component={IssueDetail} />
        <Route default component={NotFound} />
      </Router>
    </LocationProvider>
  );
}
