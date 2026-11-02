import {readFileSync} from 'node:fs';
import {testDashboardTokenExpiry} from './dashboard-token-expiry-fixture.mjs';
await testDashboardTokenExpiry(readFileSync(new URL('../internal/api/static/app.js', import.meta.url), 'utf8'));
console.log('dashboard token expiry passed: admin view, legacy rotation warnings, escaped public metadata, authorization and read errors');
