// Install jsdom@26.1.0 outside the source tree, then run with its node_modules
// on NODE_PATH: node testdata/dashboard_injection_test.cjs
// This exercises the real Go HTTP observer with synthetic config/SQLite data.
const assert = require('node:assert/strict');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const net = require('node:net');
const { spawn, spawnSync } = require('node:child_process');
const { JSDOM, VirtualConsole } = require('jsdom');

async function main() {
  const root = path.resolve(__dirname, '..');
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'looper-dashboard-test-'));
  const binary = path.join(dir, 'looper');
  const db = path.join(dir, 'fixture.db');
  const job = "job');window.__injected++;void(' & #?+=中文";
  const markup = '<img src=x onerror="window.__injected++">';
  const text = "'\" onmouseover=\"window.__injected++\" data-x=\"&<>中文";
  const id = text + '/?# +';
  let server;
  let dom;
  try {
    const build = spawnSync('go', ['build', '-p', '2', '-o', binary, '.'], { cwd: root, encoding: 'utf8' });
    assert.equal(build.status, 0, build.stderr);
    const fixture = spawnSync('python3', ['-c', `
import json, sqlite3, sys
db, markup, text, ident = sys.argv[1:]
c = sqlite3.connect(db)
c.executescript('''
CREATE TABLE reply_candidate_backlog (tweet_id TEXT, author TEXT, text TEXT, likes INTEGER, views INTEGER, score REAL, status TEXT, skip_reason TEXT, discovered_at TEXT, created_at TEXT);
CREATE TABLE reply_review_queue (id INTEGER, parent_id TEXT, author TEXT, parent_text TEXT, reply_text TEXT, status TEXT, reviewer_type TEXT, reviewer_id TEXT, reject_reason TEXT, reply_id TEXT, created_at TEXT, reviewed_at TEXT, expires_at TEXT);
CREATE TABLE published_item_tracking (reply_id TEXT, parent_id TEXT, author TEXT, skeleton TEXT, reply_style TEXT, score REAL, selection_score REAL, candidate_vr REAL, likes INTEGER, views INTEGER, target_likes INTEGER, target_views INTEGER, type TEXT, posted_at TEXT, check_stage TEXT, last_checked_at TEXT);
CREATE TABLE actions (action_type TEXT, acted_at TEXT);
CREATE TABLE round_log (id INTEGER, ts TEXT, mode TEXT, hot_topic TEXT, hot_pct REAL, foryou_total INTEGER, foryou_new INTEGER, following_total INTEGER, following_new INTEGER, candidate_backlog_pending INTEGER, replies_total INTEGER, replies_cn INTEGER, replies_en INTEGER, skeletons TEXT, skip_reasons TEXT, backlog_count INTEGER, quota_reply_used INTEGER, quota_reply_limit INTEGER, quota_hourly_used INTEGER, quota_hourly_limit INTEGER, empty_reason TEXT);
''')
c.execute("INSERT INTO reply_candidate_backlog VALUES (?, ?, ?, 1, 2, 8.5, 'pending', '', ?, '')", (ident, markup, text, markup))
c.execute("INSERT INTO reply_review_queue VALUES (1, ?, ?, ?, ?, 'pending', ?, ?, '', ?, ?, '', '')", (ident, markup, text, text, markup, text, ident, markup))
c.execute("INSERT INTO published_item_tracking VALUES (?, ?, ?, ?, ?, 8, 8.5, 10, 1, 2, 3, 4, ?, datetime('now', 'localtime'), ?, ?)", (ident, ident, markup, markup, markup, markup, markup, markup))
c.execute("INSERT INTO actions VALUES (?, datetime('now', 'localtime'))", (markup,))
c.execute("INSERT INTO round_log VALUES (1, ?, ?, ?, 10, 1, 1, 1, 1, 1, 1, 1, 0, ?, ?, 1, 1, 2, 1, 2, ?)", (markup, markup, markup, json.dumps({markup: markup}), json.dumps({markup: markup}), markup))
c.commit()
c.close()
`, db, markup, text, id], { encoding: 'utf8' });
    assert.equal(fixture.status, 0, fixture.stderr);
    const platform = { enabled: true, db, metrics: { candidate_backlog_table: 'reply_candidate_backlog', candidate_backlog_status_field: 'status', candidate_backlog_pending_value: 'pending', action_table: 'actions', action_type_field: 'action_type', action_time_field: 'acted_at' } };
    const config = path.join(dir, 'workflow.yaml');
    fs.writeFileSync(config, JSON.stringify({
      engines: { test: { kind: 'claude', cli: 'true', skills_dir: dir } },
      platforms: { x: platform, [markup]: platform },
      scheduling: { jobs: [job, markup].map(name => ({ name, type: 'script', command: ':', workdir: dir, schedule: '@yearly' })) }
    }));
    const socket = net.createServer();
    await new Promise(resolve => socket.listen(0, '127.0.0.1', resolve));
    const port = socket.address().port;
    await new Promise(resolve => socket.close(resolve));
    const base = `http://127.0.0.1:${port}`;
    server = spawn(binary, ['-config', config, '-port', String(port)], { cwd: dir, stdio: ['ignore', 'ignore', 'pipe'] });
    let serverError = '';
    server.stderr.on('data', data => { serverError += data; });
    server.on('error', error => { serverError += error.message; });
    const deadline = Date.now() + 15000;
    while (true) {
      try {
        const response = await fetch(base + '/health');
        assert.equal(response.status, 200);
        break;
      } catch (error) {
        if (server.exitCode !== null || Date.now() >= deadline) throw new Error(serverError || error.message);
        await new Promise(resolve => setTimeout(resolve, 20));
      }
    }
    const dashboard = await fetch(base + '/');
    assert.match(dashboard.headers.get('content-type'), /^text\/html/);
    const requests = [];
    const errors = [];
    const virtualConsole = new VirtualConsole();
    virtualConsole.on('jsdomError', error => { errors.push(error.message); });
    virtualConsole.on('error', (...args) => { errors.push(args.map(String).join(' ')); });
    dom = new JSDOM(await dashboard.text(), {
      url: base + '/', runScripts: 'dangerously', virtualConsole,
      beforeParse(window) {
        window.__injected = 0;
        window.setInterval = () => 1;
        window.clearInterval = () => {};
        window.fetch = async (url, options) => {
          const response = await fetch(new URL(url, base), options);
          requests.push({ url: new URL(url, base), method: options?.method || 'GET', status: response.status });
          return response;
        };
      }
    });
    const window = dom.window;
    await window.refreshStatus();
    await window.refreshJobs();
    await window.refreshReplies();
    await window.refreshCandidateBacklog();
    await window.refreshReplyReviews();
    await window.refreshTracking();
    await window.refreshRounds();
    const failures = [];
    function check(name, fn) {
      try { fn(); console.log('PASS ' + name); }
      catch (error) { failures.push(name + ': ' + error.message); console.error('FAIL ' + name + ': ' + error.message); }
    }
    const document = window.document;
    const panels = ['platforms', 'jobs', 'repliesBody', 'candidateBacklogBody', 'replyReviewsBody', 'trackingBody', 'roundsBody'];
    for (const panel of panels) {
      check(panel + ' contains rows', () => assert.ok(document.getElementById(panel).children.length));
      check(panel + ' keeps stored markup as text', () => assert.equal(document.getElementById(panel).querySelectorAll('img,script,iframe').length, 0));
      check(panel + ' has no data-created event attributes', () => {
        for (const element of document.getElementById(panel).querySelectorAll('*')) {
          assert.equal([...element.attributes].filter(attr => /^on/i.test(attr.name)).length, 0, element.outerHTML);
        }
      });
    }
    check('HTML escape preserves all five special characters', () => {
      const element = document.createElement('div');
      element.innerHTML = '<span title="' + window.esc(text) + '">' + window.esc(text) + '</span>';
      assert.equal(element.firstChild.title, text);
      assert.equal(element.firstChild.textContent, text);
      assert.equal(element.firstChild.attributes.length, 1);
    });
    check('candidate title preserves stored text', () => assert.equal(document.querySelector('#candidateBacklogBody .text-preview').title, text));
    check('review titles preserve stored text', () => {
      for (const cell of document.querySelectorAll('#replyReviewsBody .text-preview')) assert.equal(cell.title, text);
    });
    for (const panel of ['repliesBody', 'candidateBacklogBody', 'replyReviewsBody', 'trackingBody']) {
      check(panel + ' link encodes the complete record ID', () => {
        const link = document.querySelector('#' + panel + ' .tweet-link');
        assert.equal(link.getAttribute('href'), 'https://x.com/i/status/' + encodeURIComponent(id));
      });
    }
    check('platform name and action name remain visible', () => {
      assert.ok([...document.querySelectorAll('#platforms h2')].some(element => element.textContent === markup));
      assert.ok([...document.querySelectorAll('#platforms .item')].every(element => element.textContent.includes(markup)));
    });
    check('normal numeric tweet link remains unchanged', () => assert.equal(window.tweetUrl('123456'), 'https://x.com/i/status/123456'));
    for (const panel of panels) {
      for (const element of document.getElementById(panel).querySelectorAll('*')) element.dispatchEvent(new window.Event('mouseover'));
    }
    check('stored attribute payload never executes', () => assert.equal(window.__injected, 0));
    const row = [...document.querySelectorAll('#jobs tr')].find(row => row.querySelector('a').textContent === job);
    check('job name remains visible', () => assert.ok(row));
    if (row) {
      row.querySelector('a').dispatchEvent(new window.MouseEvent('click', { bubbles: true, cancelable: true }));
      await window.refreshLog();
      check('view-log action retains the literal job name', () => {
        assert.equal(document.getElementById('logTitle').textContent, job);
        assert.ok(requests.some(request => request.url.pathname === '/logs' && request.url.searchParams.get('job') === job));
      });
      row.querySelector('.btn-run').dispatchEvent(new window.MouseEvent('click', { bubbles: true }));
      const runDeadline = Date.now() + 5000;
      while (!requests.some(request => request.method === 'POST') && Date.now() < runDeadline) await new Promise(resolve => setTimeout(resolve, 10));
      check('run action sends the literal job name to the real endpoint', () => {
        const runs = requests.filter(request => request.method === 'POST');
        assert.equal(runs.length, 1);
        assert.equal(runs[0].url.searchParams.get('job'), job);
        assert.equal(runs[0].status, 200);
      });
    }
    check('stored job payload never executes', () => assert.equal(window.__injected, 0));
    check('dashboard reports no rendering errors', () => assert.deepEqual(errors, []));
    assert.equal(failures.length, 0, failures.join('\n'));
  } finally {
    if (dom) dom.window.close();
    if (server && server.exitCode === null) {
      server.kill('SIGTERM');
      await new Promise(resolve => server.once('exit', resolve));
    }
    fs.rmSync(dir, { recursive: true, force: true });
  }
}

main().catch(error => { console.error(error.message); process.exitCode = 1; });
