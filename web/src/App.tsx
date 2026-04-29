import { LocationProvider, Router, Route } from 'preact-iso';
import { Placeholder } from './pages/Placeholder';

export function App() {
  return (
    <LocationProvider>
      <Router>
        <Route path="/" component={Placeholder} />
        <Route default component={Placeholder} />
      </Router>
    </LocationProvider>
  );
}
