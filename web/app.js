/* BlastSMTP - console front-end.
   Vanilla on purpose: the whole UI ships inside the Go binary, so every
   kilobyte here is a kilobyte in the executable. Translations live in i18n.js;
   t() is used for anything built at runtime. */

'use strict';

const $ = (sel, root = document) => root.querySelector(sel);
const $$ = (sel, root = document) => Array.from(root.querySelectorAll(sel));

const TOKEN = window.BLAST_TOKEN;

// ------------------------------------------------------------- api

async function api(path, { method = 'GET', body, form } = {}) {
  const opts = { method, headers: { 'X-Blast-Token': TOKEN } };
  if (form) {
    opts.body = form;
  } else if (body !== undefined) {
    opts.headers['Content-Type'] = 'application/json';
    opts.body = JSON.stringify(body);
  }
  const res = await fetch(path, opts);
  const text = await res.text();
  let data = null;
  try { data = text ? JSON.parse(text) : null; } catch { data = { raw: text }; }
  if (!res.ok) throw new Error((data && data.error) || text || `HTTP ${res.status}`);
  return data;
}

// ------------------------------------------------------------- toasts

function toast(message, kind = 'info', ms = 4200) {
  const el = document.createElement('div');
  el.className = `toast ${kind}`;
  el.textContent = message;
  $('#toasts').appendChild(el);
  setTimeout(() => {
    el.style.transition = 'opacity .3s, transform .3s';
    el.style.opacity = '0';
    el.style.transform = 'translateX(20px)';
    setTimeout(() => el.remove(), 320);
  }, ms);
}

// ------------------------------------------------------------- state

const state = {
  profiles: [],
  activeProfile: '',
  savePasswords: false,
  list: { loaded: false, count: 0, columns: [], preview: [] },
  attachments: [],
  smtpOK: false,
  lastProbe: null,
  lastStats: null,
  previewIndex: 1,
  running: false,
  logCount: 0,
};

// ----------------------------------------------------- panel navigation

$$('.step').forEach((step) => {
  step.addEventListener('click', () => showPanel(step.dataset.panel));
});

function showPanel(name) {
  $$('.step').forEach((s) => s.classList.toggle('active', s.dataset.panel === name));
  $$('.panel').forEach((p) => p.classList.toggle('active', p.id === `panel-${name}`));
  $('.main').scrollTop = 0;
  if (name === 'send') { refreshChecklist(); refreshEstimate(); }
}

function markStep(panel, value) {
  const step = $(`.step[data-panel="${panel}"]`);
  if (step) step.dataset.ok = String(value);
}

// ------------------------------------------------------- language

$$('#lang-seg button').forEach((b) => {
  b.addEventListener('click', () => setLang(b.dataset.lang));
});

// Re-render everything that is built in JS rather than marked up in HTML.
window.addEventListener('langchange', () => {
  renderProfiles();
  renderVarPalette();
  renderAttachments();
  applyList(state.list);
  if (state.lastProbe) renderProbe(state.lastProbe);
  if (state.lastStats) applyStats(state.lastStats);
  else refreshPills();
  refreshChecklist();
  refreshEstimate();
});

// --------------------------------------------------------- smtp presets

const PRESETS = [
  { name: 'Gmail', host: 'smtp.gmail.com', port: 587, enc: 'starttls' },
  { name: 'Outlook / M365', host: 'smtp.office365.com', port: 587, enc: 'starttls' },
  { name: 'OVH', host: 'ssl0.ovh.net', port: 587, enc: 'starttls' },
  { name: 'Brevo', host: 'smtp-relay.brevo.com', port: 587, enc: 'starttls' },
  { name: 'SendGrid', host: 'smtp.sendgrid.net', port: 587, enc: 'starttls' },
  { name: 'Mailgun', host: 'smtp.mailgun.org', port: 587, enc: 'starttls' },
  { name: 'Amazon SES', host: 'email-smtp.eu-west-1.amazonaws.com', port: 587, enc: 'starttls' },
  { name: 'Postmark', host: 'smtp.postmarkapp.com', port: 587, enc: 'starttls' },
  { name: 'Zoho', host: 'smtp.zoho.eu', port: 465, enc: 'ssl' },
  { name: 'MailHog', host: 'localhost', port: 1025, enc: 'none' },
];

function renderPresets() {
  const box = $('#presets');
  box.innerHTML = '';
  PRESETS.forEach((p) => {
    const b = document.createElement('button');
    b.className = 'preset';
    b.type = 'button';
    b.textContent = p.name;
    b.onclick = () => {
      $('#smtp-host').value = p.host;
      $('#smtp-port').value = p.port;
      setEncryption(p.enc);
      refreshChecklist();
      toast(t('preset.applied', p.name), 'info', 2200);
    };
    box.appendChild(b);
  });
}

function setEncryption(v) {
  $$('#enc-seg button').forEach((b) => b.classList.toggle('on', b.dataset.v === v));
}
function getEncryption() {
  const on = $('#enc-seg button.on');
  return on ? on.dataset.v : 'starttls';
}
$$('#enc-seg button').forEach((b) => {
  b.addEventListener('click', () => {
    setEncryption(b.dataset.v);
    // Nudge the port to the conventional one for the chosen mode.
    const port = $('#smtp-port');
    if (b.dataset.v === 'ssl' && ['25', '587'].includes(port.value)) port.value = 465;
    if (b.dataset.v === 'starttls' && port.value === '465') port.value = 587;
  });
});

$('#btn-reveal').addEventListener('click', () => {
  const f = $('#smtp-pass');
  f.type = f.type === 'password' ? 'text' : 'password';
});

function smtpConfig() {
  return {
    host: $('#smtp-host').value.trim(),
    port: parseInt($('#smtp-port').value, 10) || 587,
    username: $('#smtp-user').value.trim(),
    password: $('#smtp-pass').value,
    encryption: getEncryption(),
    authMethod: $('#auth-method').value,
    fromName: $('#from-name').value.trim(),
    fromEmail: $('#from-email').value.trim(),
    replyTo: $('#reply-to').value.trim(),
    heloName: $('#helo').value.trim(),
    skipVerify: $('#skip-verify').checked,
    allowInsecureAuth: $('#insecure-auth').checked,
    timeoutSeconds: parseInt($('#timeout').value, 10) || 30,
  };
}

function fillSMTP(c) {
  if (!c) return;
  $('#smtp-host').value = c.host || '';
  $('#smtp-port').value = c.port || 587;
  $('#smtp-user').value = c.username || '';
  $('#smtp-pass').value = c.password || '';
  setEncryption(c.encryption || 'starttls');
  $('#auth-method').value = c.authMethod || 'auto';
  $('#from-name').value = c.fromName || '';
  $('#from-email').value = c.fromEmail || '';
  $('#reply-to').value = c.replyTo || '';
  $('#helo').value = c.heloName || '';
  $('#skip-verify').checked = !!c.skipVerify;
  $('#insecure-auth').checked = !!c.allowInsecureAuth;
  $('#timeout').value = c.timeoutSeconds || 30;
}

// --------------------------------------------------------- smtp test

$('#btn-test').addEventListener('click', async () => {
  const btn = $('#btn-test');
  btn.disabled = true;
  btn.textContent = t('btn.testing');
  setPill('#pill-smtp', 'busy', t('pill.smtp.testing'));
  try {
    const p = await api('/api/smtp/test', { method: 'POST', body: smtpConfig() });
    state.lastProbe = p;
    renderProbe(p);
    state.smtpOK = p.ok;
    markStep('smtp', p.ok ? 'true' : 'error');
    setPill('#pill-smtp', p.ok ? 'ok' : 'err', p.ok ? t('pill.smtp.ok') : t('pill.smtp.fail'));
    toast(p.ok ? t('probe.connected', p.latencyMs) : t('probe.refused'), p.ok ? 'ok' : 'err');
  } catch (e) {
    state.lastProbe = { ok: false, error: e.message };
    renderProbe(state.lastProbe);
    state.smtpOK = false;
    markStep('smtp', 'error');
    setPill('#pill-smtp', 'err', t('pill.smtp.fail'));
    toast(e.message, 'err', 6000);
  } finally {
    btn.disabled = false;
    btn.textContent = t('btn.test');
    refreshChecklist();
  }
});

function renderProbe(p) {
  const box = $('#probe');
  if (!p) { box.innerHTML = ''; return; }
  if (!p.ok) {
    box.innerHTML = `<div class="probe ko">
      <div class="headline">${esc(t('probe.fail'))}</div>
      <div class="probe-error">${esc(p.error || '')}</div>
      <div class="probe-hints">${esc(t('probe.hints'))}</div>
    </div>`;
    return;
  }
  const exts = (p.extensions || []).map((e) => `<span class="ext">${esc(e)}</span>`).join('');
  box.innerHTML = `<div class="probe ok">
    <div class="headline">${esc(t('probe.ok'))}</div>
    <dl>
      <dt>${esc(t('probe.latency'))}</dt><dd>${p.latencyMs} ms</dd>
      <dt>${esc(t('probe.tls'))}</dt><dd>${p.tls ? esc(p.tlsVersion) + ' · ' + esc(p.cipher) : esc(t('probe.noTls'))}</dd>
      <dt>${esc(t('probe.mechanisms'))}</dt><dd>${esc(p.auth || '-')}</dd>
      <dt>${esc(t('probe.maxSize'))}</dt><dd>${p.maxSize ? fmtBytes(p.maxSize) : '-'}</dd>
      <dt>${esc(t('probe.extensions'))}</dt><dd>${exts || '-'}</dd>
    </dl>
  </div>`;
}

function setPill(sel, st, text) {
  const el = $(sel);
  el.dataset.state = st;
  el.lastElementChild.textContent = text;
}

function refreshPills() {
  const n = state.list.count || 0;
  setPill('#pill-list', n ? 'ok' : '', n ? t('pill.list.count', n) : t('pill.list.none'));
  setPill('#pill-smtp', state.smtpOK ? 'ok' : '', state.smtpOK ? t('pill.smtp.ok') : t('pill.smtp.untested'));
}

// --------------------------------------------------------- profiles

function renderProfiles() {
  const sel = $('#profile-select');
  sel.innerHTML = '';
  const blank = document.createElement('option');
  blank.value = '';
  blank.textContent = t('profile.new');
  sel.appendChild(blank);
  state.profiles.forEach((p) => {
    const o = document.createElement('option');
    o.value = p.id;
    o.textContent = p.name;
    sel.appendChild(o);
  });
  sel.value = state.activeProfile || '';
}

$('#profile-select').addEventListener('change', (e) => {
  const p = state.profiles.find((x) => x.id === e.target.value);
  state.activeProfile = e.target.value;
  if (p) {
    fillSMTP(p.smtp);
    $('#profile-name').value = p.name;
    refreshChecklist();
    toast(t('profile.loaded', p.name), 'info', 2200);
  }
});

$('#btn-profile-save').addEventListener('click', async () => {
  const name = $('#profile-name').value.trim() || $('#smtp-host').value.trim();
  if (!name) return toast(t('profile.needName'), 'err');
  const id = state.activeProfile || `p${Date.now().toString(36)}`;
  const existing = state.profiles.find((p) => p.id === id);
  if (existing) { existing.name = name; existing.smtp = smtpConfig(); }
  else state.profiles.push({ id, name, smtp: smtpConfig() });
  state.activeProfile = id;
  renderProfiles();
  await saveConfig(true);
});

$('#btn-profile-delete').addEventListener('click', async () => {
  if (!state.activeProfile) return;
  state.profiles = state.profiles.filter((p) => p.id !== state.activeProfile);
  state.activeProfile = '';
  $('#profile-name').value = '';
  renderProfiles();
  await saveConfig(true);
});

// --------------------------------------------------------- config i/o

async function loadConfig() {
  const { config, configPath, version } = await api('/api/config');
  $('#config-path').textContent = configPath;
  if (version) $('#version-chip').textContent = `v${version}`;
  state.profiles = config.profiles || [];
  state.activeProfile = config.activeProfile || '';
  state.savePasswords = !!config.savePasswords;
  $('#save-passwords').checked = state.savePasswords;
  renderProfiles();

  const active = state.profiles.find((p) => p.id === state.activeProfile);
  if (active) { fillSMTP(active.smtp); $('#profile-name').value = active.name; }

  const d = config.draft || {};
  $('#subject').value = d.subject || '';
  $('#body-html').value = d.html || '';
  $('#body-text').value = d.text || '';
  renderHeaders(d.headers || {});

  const s = config.sending || {};
  $('#workers').value = s.workers || 2;
  $('#rate').value = s.ratePerMinute ?? 60;
  $('#batch').value = s.batchSize ?? 50;
  $('#batch-pause').value = s.batchPauseSeconds ?? 30;
  $('#retries').value = s.maxRetries ?? 1;
  $('#reconnect').value = s.reconnectEvery ?? 100;
  $('#index-start').value = s.indexStart || 1;
  $('#stop-on-error').checked = !!s.stopOnError;
}

async function saveConfig(loud = false) {
  const payload = {
    version: 1,
    savePasswords: state.savePasswords,
    profiles: state.profiles,
    activeProfile: state.activeProfile,
    draft: {
      subject: $('#subject').value,
      html: $('#body-html').value,
      text: $('#body-text').value,
      headers: collectHeaders(),
    },
    sending: sendingConfig(),
  };
  try {
    await api('/api/config', { method: 'POST', body: payload });
    if (loud) toast(t('config.saved'), 'ok', 2200);
  } catch (e) {
    if (loud) toast(e.message, 'err');
  }
}

$('#btn-save-config').addEventListener('click', () => saveConfig(true));

$('#save-passwords').addEventListener('change', (e) => {
  state.savePasswords = e.target.checked;
  if (!state.savePasswords) toast(t('savepass.cleared'), 'info');
  saveConfig(false);
});

// Autosave: the draft is the expensive thing to lose, not the credentials.
let saveTimer = null;
function scheduleSave() {
  clearTimeout(saveTimer);
  saveTimer = setTimeout(() => saveConfig(false), 2500);
}
['#subject', '#body-html', '#body-text'].forEach((sel) =>
  $(sel).addEventListener('input', scheduleSave)
);

// --------------------------------------------------------- recipients

function wireDropzone(zoneSel, inputSel, handler) {
  const zone = $(zoneSel);
  const input = $(inputSel);
  zone.addEventListener('click', () => input.click());
  input.addEventListener('change', () => {
    if (input.files.length) handler(input.files);
    input.value = '';
  });
  ['dragenter', 'dragover'].forEach((ev) =>
    zone.addEventListener(ev, (e) => { e.preventDefault(); zone.classList.add('over'); })
  );
  ['dragleave', 'drop'].forEach((ev) =>
    zone.addEventListener(ev, (e) => { e.preventDefault(); zone.classList.remove('over'); })
  );
  zone.addEventListener('drop', (e) => {
    if (e.dataTransfer.files.length) handler(e.dataTransfer.files);
  });
}

wireDropzone('#drop-list', '#file-list', async (files) => {
  const form = new FormData();
  form.append('file', files[0]);
  try {
    const res = await api('/api/recipients', { method: 'POST', form });
    applyList(res);
    toast(t('list.imported', res.count), 'ok');
  } catch (e) {
    toast(e.message, 'err', 6000);
  }
});

$('#btn-clear-list').addEventListener('click', async () => {
  applyList(await api('/api/recipients', { method: 'DELETE' }));
  toast(t('list.cleared'), 'info', 2000);
});

function applyList(res) {
  state.list = res || { loaded: false, count: 0, columns: [], preview: [] };
  const has = state.list.loaded && state.list.count > 0;
  $('#list-results').style.display = has ? 'block' : 'none';
  markStep('list', has ? 'true' : 'false');
  setPill('#pill-list', has ? 'ok' : '', has ? t('pill.list.count', state.list.count) : t('pill.list.none'));

  if (!has) {
    $('#list-filename').textContent = t('list.noFile');
    renderVarPalette();
    refreshChecklist();
    refreshEstimate();
    return;
  }

  const r = state.list;
  let meta = t('list.meta', r.filename, r.format);
  if (r.delimiter) meta += t('list.metaSep', r.delimiter === '\t' ? 'tab' : r.delimiter);
  if (r.hasHeader) meta += t('list.metaHeader');
  $('#list-filename').textContent = meta;

  $('#t-valid').textContent = r.count;
  $('#t-dupes').textContent = r.duplicates || 0;
  $('#t-rejected').textContent = (r.rejected || []).length;
  $('#t-columns').textContent = (r.columns || []).length;

  const cols = r.columns || [];
  const preview = r.preview || [];
  $('#preview-count').textContent = t('list.previewCount', Math.min(preview.length, r.count), r.count);

  $('#list-table thead').innerHTML =
    '<tr><th>#</th>' + cols.map((c) => `<th>${esc(c)}</th>`).join('') + '</tr>';
  $('#list-table tbody').innerHTML = preview
    .map((row, i) => `<tr><td class="idx">${i + 1}</td>${cols.map((c) => `<td>${esc(row.fields[c] || '')}</td>`).join('')}</tr>`)
    .join('');

  const rejected = r.rejected || [];
  $('#rejected-card').style.display = rejected.length ? 'block' : 'none';
  $('#rejected-table tbody').innerHTML = rejected
    .slice(0, 300)
    .map((x) => `<tr><td class="idx">${x.line}</td><td>${esc(x.raw)}</td><td>${esc(x.reason)}</td></tr>`)
    .join('');

  const colBox = $('#list-columns');
  colBox.innerHTML = '';
  cols.forEach((c) => colBox.appendChild(varChip(`{{${c}}}`, t('var.column', c))));

  renderVarPalette();
  refreshChecklist();
  refreshEstimate();
}

// ----------------------------------------------------- variable palette

// [token, description key] - the token itself stays untranslated, it is syntax.
const BUILTINS = [
  ['{{index}}', 'var.index'],
  ['{{index:1000}}', 'var.indexStart'],
  ['{{count}}', 'var.count'],
  ['{{date}}', 'var.date'],
  ['{{date:DD MMMM YYYY}}', 'var.dateFmt'],
  ['{{time}}', 'var.time'],
  ['{{year}}', 'var.year'],
  ['{{rand:1000-9999}}', 'var.rand'],
  ['{{randstr:8}}', 'var.randstr'],
  ['{{randnum:6}}', 'var.randnum'],
  ['{{uuid}}', 'var.uuid'],
  ['{{spin:A;B;C}}', 'var.spin'],
  ['{{upper:nom}}', 'var.upper'],
  ['{{capitalize:nom}}', 'var.capitalize'],
  ['{{emailuser}}', 'var.emailuser'],
  ['{{emaildomain}}', 'var.emaildomain'],
  ['{{nom|Client}}', 'var.fallback'],
];

function varChip(token, title) {
  const b = document.createElement('button');
  b.className = 'varchip';
  b.type = 'button';
  b.title = title;
  b.innerHTML = `<b>${esc(token)}</b>`;
  b.onclick = () => insertAtCursor(token);
  return b;
}

function renderVarPalette() {
  const box = $('#var-palette');
  box.innerHTML = '';
  (state.list.columns || []).forEach((c) => box.appendChild(varChip(`{{${c}}}`, t('var.column', c))));
  BUILTINS.forEach(([token, key]) => box.appendChild(varChip(token, t(key))));
}

// The palette inserts into whichever text field was last focused.
let lastField = null;
['#subject', '#body-html', '#body-text', '#unsubscribe'].forEach((sel) => {
  const el = $(sel);
  el.addEventListener('focus', () => { lastField = el; });
});

function insertAtCursor(text) {
  const el = lastField || $('#body-html');
  el.focus();
  const start = el.selectionStart ?? el.value.length;
  const end = el.selectionEnd ?? el.value.length;
  el.value = el.value.slice(0, start) + text + el.value.slice(end);
  el.selectionStart = el.selectionEnd = start + text.length;
  el.dispatchEvent(new Event('input'));
}

// --------------------------------------------------------- compose tabs

$$('.tab').forEach((tab) => {
  tab.addEventListener('click', () => {
    $$('.tab').forEach((x) => x.classList.toggle('on', x === tab));
    $$('.tabpane').forEach((p) => p.classList.toggle('on', p.dataset.pane === tab.dataset.tab));
    if (tab.dataset.tab === 'preview') refreshPreview();
  });
});

$('#btn-strip').addEventListener('click', () => {
  const html = $('#body-html').value;
  const text = html
    .replace(/<br\s*\/?>/gi, '\n')
    .replace(/<\/(p|div|h[1-6]|li|tr)>/gi, '\n')
    .replace(/<li[^>]*>/gi, '- ')
    .replace(/<[^>]+>/g, '')
    .replace(/&nbsp;/gi, ' ')
    .replace(/&amp;/gi, '&')
    .replace(/&lt;/gi, '<')
    .replace(/&gt;/gi, '>')
    .replace(/\n{3,}/g, '\n\n')
    .trim();
  $('#body-text').value = text;
  scheduleSave();
  refreshChecklist();
  toast(t('strip.done'), 'ok', 2200);
});

// ------------------------------------------------------------- preview

async function refreshPreview() {
  try {
    const res = await api('/api/preview', {
      method: 'POST',
      body: {
        subject: $('#subject').value,
        html: $('#body-html').value,
        text: $('#body-text').value,
        index: state.previewIndex,
        indexStart: parseInt($('#index-start').value, 10) || 1,
        seed: 1,
      },
    });
    state.previewIndex = res.index;
    $('#prev-pos').textContent = `${res.index} / ${res.total}`;
    $('#prev-to').textContent = res.to || '-';
    $('#prev-subject').textContent = res.subject || '-';
    $('#prev-frame').srcdoc = res.html && res.html.trim()
      ? res.html
      : `<pre style="font:13px/1.6 ui-monospace,monospace;white-space:pre-wrap;padding:16px">${esc(res.text || '')}</pre>`;

    const warn = $('#prev-warn');
    warn.innerHTML = res.missing && res.missing.length
      ? `<div class="warn">${esc(t('prev.missing', res.missing.join(', ')))}</div>`
      : '';
  } catch (e) {
    toast(e.message, 'err');
  }
}

$('#btn-refresh-preview').addEventListener('click', refreshPreview);
$('#prev-back').addEventListener('click', () => { state.previewIndex = Math.max(1, state.previewIndex - 1); refreshPreview(); });
$('#prev-next').addEventListener('click', () => { state.previewIndex += 1; refreshPreview(); });

$('#btn-send-test').addEventListener('click', async () => {
  const to = $('#test-to').value.trim();
  if (!to) return toast(t('test.needAddress'), 'err');
  const btn = $('#btn-send-test');
  btn.disabled = true;
  btn.textContent = t('btn.sending');
  try {
    const res = await api('/api/send-test', {
      method: 'POST',
      body: {
        smtp: smtpConfig(), to,
        subject: $('#subject').value, html: $('#body-html').value, text: $('#body-text').value,
        headers: collectHeaders(),
        index: state.previewIndex,
        indexStart: parseInt($('#index-start').value, 10) || 1,
      },
    });
    if (res.ok) toast(t('test.delivered', res.durationMs), 'ok');
    else toast(res.error, 'err', 7000);
  } catch (e) {
    toast(e.message, 'err', 7000);
  } finally {
    btn.disabled = false;
    btn.textContent = t('btn.sendTest');
  }
});

// --------------------------------------------------------- attachments

wireDropzone('#drop-files', '#file-attach', async (files) => {
  const form = new FormData();
  Array.from(files).forEach((f) => form.append('files', f));
  try {
    state.attachments = await api('/api/attachments', { method: 'POST', form });
    renderAttachments();
    toast(t('attach.added', files.length), 'ok');
  } catch (e) {
    toast(e.message, 'err', 6000);
  }
});

function renderAttachments() {
  const box = $('#attach-list');
  box.innerHTML = '';
  state.attachments.forEach((a) => {
    const row = document.createElement('div');
    row.className = 'file-row';
    row.innerHTML = `
      <span class="nm">${esc(a.filename)}</span>
      <span class="sz">${fmtBytes(a.size)}</span>
      <span class="sz hide-sm">${esc(a.mimeType || '')}</span>`;

    if ((a.mimeType || '').startsWith('image/')) {
      const toggle = document.createElement('button');
      toggle.className = 'btn sm ' + (a.inline ? 'signal' : 'ghost');
      toggle.textContent = a.inline ? t('attach.inline') : t('attach.attached');
      toggle.title = a.inline ? t('attach.inlineHint', a.contentId) : t('attach.toggleHint');
      toggle.onclick = async () => {
        state.attachments = await api(`/api/attachments/${encodeURIComponent(a.filename)}/inline`, { method: 'POST' });
        renderAttachments();
      };
      row.appendChild(toggle);

      if (a.inline) {
        const copy = document.createElement('button');
        copy.className = 'btn sm';
        copy.textContent = 'cid:';
        copy.onclick = () => insertAtCursor(`<img src="cid:${a.contentId}" alt="${a.filename}" />`);
        row.appendChild(copy);
      }
    }

    const del = document.createElement('button');
    del.className = 'btn sm danger';
    del.textContent = '✕';
    del.onclick = async () => {
      state.attachments = await api(`/api/attachments/${encodeURIComponent(a.filename)}`, { method: 'DELETE' });
      renderAttachments();
    };
    row.appendChild(del);
    box.appendChild(row);
  });
}

// ------------------------------------------------------- custom headers

function renderHeaders(headers) {
  const box = $('#headers-rows');
  box.innerHTML = '';
  const entries = Object.entries(headers || {});
  if (!entries.length) entries.push(['', '']);
  entries.forEach(([k, v]) => box.appendChild(headerRow(k, v)));
}

function headerRow(k = '', v = '') {
  const row = document.createElement('div');
  row.className = 'kv-row';
  row.innerHTML = `<input type="text" placeholder="${esc(t('header.key.ph'))}" value="${esc(k)}" />
                   <input type="text" placeholder="${esc(t('header.value.ph'))}" value="${esc(v)}" />`;
  const del = document.createElement('button');
  del.className = 'btn sm danger';
  del.textContent = '✕';
  del.onclick = () => row.remove();
  row.appendChild(del);
  return row;
}

$('#btn-add-header').addEventListener('click', () => $('#headers-rows').appendChild(headerRow()));

function collectHeaders() {
  const out = {};
  $$('#headers-rows .kv-row').forEach((row) => {
    const [k, v] = $$('input', row);
    if (k.value.trim()) out[k.value.trim()] = v.value;
  });
  return out;
}

// --------------------------------------------------------- sending prefs

function sendingConfig() {
  return {
    workers: parseInt($('#workers').value, 10) || 1,
    ratePerMinute: parseInt($('#rate').value, 10) || 0,
    batchSize: parseInt($('#batch').value, 10) || 0,
    batchPauseSeconds: parseInt($('#batch-pause').value, 10) || 0,
    maxRetries: parseInt($('#retries').value, 10) || 0,
    reconnectEvery: parseInt($('#reconnect').value, 10) || 0,
    indexStart: parseInt($('#index-start').value, 10) || 1,
    stopOnError: $('#stop-on-error').checked,
  };
}

['#workers', '#rate', '#batch', '#batch-pause', '#retries', '#reconnect', '#index-start'].forEach((sel) =>
  $(sel).addEventListener('input', () => { refreshEstimate(); scheduleSave(); })
);

function refreshEstimate() {
  const n = state.list.count || 0;
  const s = sendingConfig();
  if (!n) { $('#estimate').textContent = t('estimate.none'); return; }

  let seconds = s.ratePerMinute > 0
    ? (n / s.ratePerMinute) * 60
    : (n / Math.max(1, s.workers)) * 0.4; // optimistic floor when uncapped
  if (s.batchSize > 0 && s.batchPauseSeconds > 0) {
    seconds += Math.floor(n / s.batchSize) * s.batchPauseSeconds;
  }
  const pace = s.ratePerMinute > 0 ? t('estimate.rate', s.ratePerMinute) : t('estimate.norate');
  $('#estimate').textContent = t('estimate.value', fmtDuration(seconds), n, pace, s.workers);
}

function refreshChecklist() {
  const listLabel = t('check.list') + (state.list.count ? ` (${state.list.count})` : '');
  const items = [
    [!!$('#smtp-host').value.trim() && !!$('#from-email').value.trim(), t('check.smtpFields')],
    [state.smtpOK, t('check.smtpTested')],
    [state.list.count > 0, listLabel],
    [!!$('#subject').value.trim(), t('check.subject')],
    [!!($('#body-html').value.trim() || $('#body-text').value.trim()), t('check.body')],
    [!!$('#unsubscribe').value.trim(), t('check.unsub')],
  ];
  $('#checklist').innerHTML = items
    .map(([ok, label]) => `<div class="${ok ? 'ok' : 'ko'}"><span class="mark">${ok ? '✓' : '!'}</span>${esc(label)}</div>`)
    .join('');

  const composeOK = !!$('#subject').value.trim() && !!($('#body-html').value.trim() || $('#body-text').value.trim());
  markStep('compose', composeOK ? 'true' : 'false');
}

['#subject', '#body-html', '#body-text', '#unsubscribe', '#smtp-host', '#from-email'].forEach((sel) =>
  $(sel).addEventListener('input', refreshChecklist)
);

// ------------------------------------------------------------- campaign

$('#btn-launch').addEventListener('click', async () => {
  const dry = $('#dry-run').checked;
  if (!dry && !confirm(t('launch.confirm', state.list.count))) return;

  await saveConfig(false);
  try {
    await api('/api/campaign/start', {
      method: 'POST',
      body: {
        smtp: smtpConfig(),
        subject: $('#subject').value,
        html: $('#body-html').value,
        text: $('#body-text').value,
        headers: collectHeaders(),
        sending: sendingConfig(),
        unsubscribe: $('#unsubscribe').value.trim(),
        dryRun: dry,
      },
    });
    $('#dashboard').style.display = 'block';
    $('#log').innerHTML = '';
    state.logCount = 0;
    $('#dashboard').scrollIntoView({ behavior: 'smooth', block: 'start' });
    toast(dry ? t('launch.simStarted') : t('launch.started'), 'ok');
  } catch (e) {
    toast(e.message, 'err', 7000);
  }
});

$('#btn-pause').addEventListener('click', () => api('/api/campaign/pause', { method: 'POST' }));
$('#btn-resume').addEventListener('click', () => api('/api/campaign/resume', { method: 'POST' }));
$('#btn-stop').addEventListener('click', () => {
  if (confirm(t('stop.confirm'))) api('/api/campaign/stop', { method: 'POST' });
});
$('#btn-report').addEventListener('click', () => {
  window.location.href = `/api/campaign/report.csv?token=${encodeURIComponent(TOKEN)}&lang=${currentLang()}`;
});

function connectStream() {
  const es = new EventSource(`/api/campaign/stream?token=${encodeURIComponent(TOKEN)}`);
  es.onmessage = (ev) => {
    let e;
    try { e = JSON.parse(ev.data); } catch { return; }
    if (e.stats) applyStats(e.stats);
    if (e.log) appendLog(e.log);
  };
  es.onerror = () => {
    es.close();
    // The Go server is local; a drop means it restarted or the tab slept.
    setTimeout(connectStream, 3000);
  };
}

function applyStats(s) {
  const wasRunning = state.running;
  const isNew = state.lastStats !== s;
  state.lastStats = s;
  state.running = s.state === 'running' || s.state === 'paused';

  $('#d-sent').textContent = s.sent;
  $('#d-failed').textContent = s.failed;
  $('#d-pending').textContent = s.pending;
  $('#d-rate').textContent = Math.round(s.ratePerMin || 0);
  $('#d-retries').textContent = s.retries || 0;
  $('#d-state').textContent = t(`state.${s.state}`) + (s.dryRun ? t('state.simSuffix') : '');
  $('#d-elapsed').textContent = fmtClock((s.elapsedMs || 0) / 1000);
  $('#d-eta').textContent = s.etaSeconds ? fmtClock(s.etaSeconds) : '-';

  const pct = Math.round(s.progress || 0);
  $('#progress-bar').style.width = `${pct}%`;
  $('#progress-pct').textContent = `${pct}%`;
  $('#progress').classList.toggle('live', s.state === 'running');

  $('#btn-pause').disabled = s.state !== 'running';
  $('#btn-resume').disabled = s.state !== 'paused';
  $('#btn-stop').disabled = !state.running;
  $('#btn-launch').disabled = state.running;

  if (s.total > 0 && $('#dashboard').style.display === 'none') $('#dashboard').style.display = 'block';

  setPill('#pill-smtp',
    s.state === 'running' ? 'busy' : (state.smtpOK ? 'ok' : ''),
    s.state === 'running' ? t('pill.smtp.sending', s.sent, s.total)
      : (state.smtpOK ? t('pill.smtp.ok') : t('pill.smtp.untested')));

  if (isNew && wasRunning && !state.running) {
    markStep('send', s.failed > 0 ? 'error' : 'true');
    const label = s.state === 'stopped' ? t('campaign.stopped') : t('campaign.done');
    toast(t('campaign.summary', label, s.sent, s.failed), s.failed ? 'err' : 'ok', 8000);
  }
  if (isNew && s.error) toast(s.error, 'err', 8000);
}

function appendLog(l) {
  const box = $('#log');
  if (state.logCount === 0) box.innerHTML = '';
  state.logCount++;

  const line = document.createElement('div');
  line.className = `log-line ${l.status}`;
  const label = { sent: 'OK', failed: 'KO', retry: '↻' }[l.status] || '·';
  line.innerHTML =
    `<span class="t">${esc((l.at || '').slice(11, 19))}</span>` +
    `<span class="s">${label}</span>` +
    `<span class="m">${esc(l.email)}${l.message ? ' - ' + esc(l.message) : ''}</span>` +
    `<span class="d">${l.durationMs ? l.durationMs + 'ms' : ''}</span>`;

  const atBottom = box.scrollTop + box.clientHeight >= box.scrollHeight - 40;
  box.appendChild(line);
  // Keep the DOM bounded on six-figure campaigns.
  while (box.childElementCount > 500) box.removeChild(box.firstElementChild);
  if (atBottom) box.scrollTop = box.scrollHeight;
}

// ------------------------------------------------------------- helpers

function esc(s) {
  return String(s ?? '').replace(/[&<>"']/g, (c) =>
    ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));
}

function fmtBytes(n) {
  if (n < 1024) return `${n} o`;
  if (n < 1024 * 1024) return `${(n / 1024).toFixed(1)} Ko`;
  return `${(n / 1048576).toFixed(1)} Mo`;
}

function fmtClock(seconds) {
  seconds = Math.max(0, Math.round(seconds));
  const h = Math.floor(seconds / 3600);
  const m = Math.floor((seconds % 3600) / 60);
  const s = seconds % 60;
  const pad = (v) => String(v).padStart(2, '0');
  return h ? `${h}:${pad(m)}:${pad(s)}` : `${pad(m)}:${pad(s)}`;
}

function fmtDuration(seconds) {
  if (seconds < 60) return `${Math.round(seconds)} s`;
  if (seconds < 3600) return `${Math.round(seconds / 60)} min`;
  return `${(seconds / 3600).toFixed(1)} h`;
}

// ------------------------------------------------------------- boot

(async function boot() {
  applyI18n();
  $$('#lang-seg button').forEach((b) => b.classList.toggle('on', b.dataset.lang === currentLang()));

  renderPresets();
  renderVarPalette();
  refreshPills();

  try {
    await loadConfig();
    applyList(await api('/api/recipients'));
    state.attachments = await api('/api/attachments');
    renderAttachments();
    const st = await api('/api/campaign/state');
    if (st.stats && st.stats.total > 0) {
      $('#dashboard').style.display = 'block';
      (st.logs || []).forEach(appendLog);
      applyStats(st.stats);
    }
  } catch (e) {
    toast(t('init.error', e.message), 'err', 8000);
  }
  connectStream();
  refreshChecklist();
  refreshEstimate();
})();
