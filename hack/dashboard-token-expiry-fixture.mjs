// Runs the shipped status renderer without network access or a browser.
export async function testDashboardTokenExpiry(source) {
  const start = source.indexOf('function ledFor(value)');
  const end = source.indexOf('/* ---------- router ---------- */', start);
  const escapeStart = source.indexOf('function esc(v)');
  const escapeEnd = source.indexOf('function enc(v)', escapeStart);
  if (start < 0 || end < start || escapeStart < 0 || escapeEnd < escapeStart) throw new Error('status renderer missing');
  const render = new Function('data', `return (async () => {
    let html = '', requests = [];
    const state = {status:{}}, document = {body:{dataset:{version:'test'}}};
    const slowPollMs = 1, setChrome = () => {}, setAuto = () => {}, currentRoute = () => true;
    const replaceApp = value => {html=value}, loadStatus = async () => {}, loadRepos = async () => {}, ago = value => value;
    const api = async path => {
      requests.push(path);
      if(data.forbidden) throw Object.assign(new Error('forbidden'), {status:403});
      if(data.unavailable) throw new Error('unavailable');
      return data;
    };
    ${source.slice(escapeStart, escapeEnd)}
    ${source.slice(start, end)}
    await renderStatus(0);
    return {html,requests};
  })()`);
  const expect = (condition, message) => {if (!condition) throw new Error(message)};
  const result = await render({uplinks:[{identity:'legacy<script>', fingerprint:'SHA256:<unsafe>', no_expiry:true}, {identity:'bounded-token', no_expiry:false}]});
  expect(result.requests.includes('/api/uplinks'), 'status did not request the authorized uplink view');
  expect(result.html.includes('NO EXPIRY') && result.html.includes('rotate'), 'legacy token has no rotation warning');
  expect(result.html.includes('legacy&lt;script&gt;') && !result.html.includes('legacy<script>'), 'identity is not escaped');
  expect(result.html.includes('SHA256:&lt;unsafe&gt;'), 'fingerprint is not escaped');
  expect(!result.html.includes('bounded-token'), 'bounded token incorrectly flagged');
  expect(!(await render({uplinks:[]})).html.includes('Uplink tokens without expiry'), 'empty legacy list flagged');
  expect(!(await render({forbidden:true})).html.includes('Uplink tokens without expiry'), 'non-admin warning leaked');
  expect((await render({unavailable:true})).html.includes('Could not check tokens without an expiry'), 'failed check silently hidden');
}
