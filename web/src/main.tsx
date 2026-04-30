import { render } from 'preact';
import { App } from './App';
import './styles.css';
import { applyThemePreference, readThemePreference } from './theme';

applyThemePreference(readThemePreference());

const root = document.getElementById('root');
if (!root) {
  throw new Error('#root element missing from index.html');
}

render(<App />, root);
