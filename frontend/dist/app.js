'use strict';

// Wails injects window.go (bindings) and window.runtime (events) at load.
const api = () => window.go.main.App;
const $ = (sel, root = document) => root.querySelector(sel);

const shell = $('#shell');
const card = $('#card');
const panel = $('#panel');

const state = {
  open: null,        // 'record' | 'api' | 'text' | 'history' | 'settings' | null
  busy: false,       // a window animation is running
  lastHtml: '',      // last rendered panel markup, to skip useless re-renders
  phase: 'idle',     // dictation: idle | recording | processing
  capturing: null,   // action id waiting for a new hotkey
  testing: null,     // source ('mic' | 'system') of the panel's running test recording
  live: {},          // last dictation state from the app
  hotkey: [],
  hasKey: true,
  history: [],
};

const esc = (s) => String(s ?? '').replace(/[&<>"']/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));
const icon = (id, cls = 'ico') => `<svg class="${cls}"><use href="#i-${id}"/></svg>`;
const wait = (ms) => new Promise((r) => setTimeout(r, ms));

// ---------------------------------------------------------------- feedback

let toastTimer;
function toast(msg, isErr = false) {
  const t = $('#toast');
  t.textContent = msg;
  t.classList.toggle('err', isErr);
  t.classList.add('show');
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => t.classList.remove('show'), isErr ? 5000 : 2600);
}

let brandTimer;
function brandMessage(msg, ms = 3200) {
  const brand = $('.brand');
  $('#brandMsg').textContent = msg;
  brand.classList.add('msg');
  clearTimeout(brandTimer);
  brandTimer = setTimeout(() => brand.classList.remove('msg'), ms);
}

function notify(msg, isErr = false) {
  if (state.open) toast(msg, isErr);
  else brandMessage(msg);
}

async function call(fn, ...args) {
  try {
    return await api()[fn](...args);
  } catch (e) {
    notify(String(e?.message ?? e), true);
    throw e;
  }
}

// ---------------------------------------------------------------- panels

// Window heights in CSS px, including the transparent margin (width is fixed in app.go).
const MAX_PANEL_H = 740; // taller panels scroll
const DIALOG_H = 320;

// fittedHeight is the window height that shows the open panel's content
// without empty space below it.
function fittedHeight() {
  const view = panel.querySelector('.view');
  if (!view) return MAX_PANEL_H;
  const px = (el, prop) => parseFloat(getComputedStyle(el)[prop]) || 0;
  const h = px(shell, 'paddingTop') + px(shell, 'paddingBottom')
    + px(card, 'borderTopWidth') + px(card, 'borderBottomWidth')
    + $('.bar').offsetHeight
    + px(panel, 'borderTopWidth') + px(panel, 'paddingTop') + px(panel, 'paddingBottom')
    + view.offsetHeight;
  return Math.min(MAX_PANEL_H, Math.ceil(h));
}

// fitWindow resizes the expanded window when the content height changed.
function fitWindow() {
  if (!state.open || modal.open) return Promise.resolve();
  const h = fittedHeight();
  return Math.abs(h - window.innerHeight) > 2 ? api().Expand(h) : Promise.resolve();
}

function fadeOutPanel(ms) {
  const a = panel.animate(
    [{ opacity: 1, transform: 'none' }, { opacity: 0, transform: 'translateY(-6px)' }],
    { duration: ms, easing: 'cubic-bezier(.4, 0, 1, 1)', fill: 'forwards' },
  );
  return a.finished;
}

async function openPanel(name, toggle = true) {
  if (state.busy) return;
  if (state.open === name) return toggle ? closePanel() : undefined;
  leavePanel();

  state.busy = true;
  try {
    const html = await renderView(name);
    const first = !state.open;
    state.open = name;
    state.lastHtml = html;
    setActiveButton(name);

    if (first) {
      panel.innerHTML = html;
      // Let the content start rising while the window is still growing.
      const grow = api().Expand(fittedHeight());
      setTimeout(() => card.classList.add('open'), 110);
      await grow;
    } else {
      // Cross-fade between panels while expanded.
      await fadeOutPanel(150);
      card.classList.remove('open');
      panel.innerHTML = html;
      panel.scrollTop = 0;
      panel.getAnimations().forEach((a) => a.cancel());
      const resize = fitWindow();
      void panel.offsetWidth; // restart the entrance animation
      card.classList.add('open');
      await resize;
    }
    bindView(name);
  } catch (e) {
    notify(String(e?.message ?? e), true);
  } finally {
    state.busy = false;
  }
}

async function closePanel() {
  if (!state.open || state.busy) return;
  leavePanel();
  state.busy = true;
  try {
    await fadeOutPanel(170);
    card.classList.remove('open');
    state.open = null;
    setActiveButton(null);
    await api().Collapse();
    panel.getAnimations().forEach((a) => a.cancel());
    panel.innerHTML = '';
  } finally {
    state.busy = false;
  }
}

// leavePanel stops what the open panel started.
function leavePanel() {
  if (state.open === 'result') api().ResultClosed();
  if (state.capturing) api().CancelHotkeyCapture();
  if (state.testing && state.phase === 'recording') api().TestStop();
  state.capturing = null;
}

function setActiveButton(name) {
  document.querySelectorAll('[data-panel]').forEach((b) => b.classList.toggle('active', b.dataset.panel === name));
}

function renderView(name) {
  switch (name) {
    case 'record': return renderRecord();
    case 'listen': return renderListen();
    case 'translate': return renderTranslate();
    case 'result': return renderResult();
    case 'api': return renderAPI();
    case 'text': return renderText();
    case 'history': return renderHistory();
    case 'settings': return renderSettings();
  }
  return Promise.reject(new Error(`unknown panel ${name}`));
}

function bindView(name) {
  bindCommon();
  switch (name) {
    case 'translate': return bindTranslate();
    case 'result': return bindResult();
    case 'api': return bindAPI();
    case 'text': return bindText();
    case 'history': return bindHistory();
    case 'settings': return bindSettings();
  }
}

// Re-render the open panel in place (no entrance animation) when data changed.
async function refresh(force = false) {
  const name = state.open;
  if (!name || state.busy) return;
  const html = await renderView(name);
  if (name !== state.open) return;
  if (!force && html === state.lastHtml) return;
  state.lastHtml = html;
  const scroll = panel.scrollTop;
  panel.innerHTML = html;
  panel.querySelector('.view')?.classList.add('static');
  panel.scrollTop = scroll;
  bindView(name);
  await fitWindow();
}

// ---------------------------------------------------------------- shared controls

const vh = (ic, title, sub) => `
  <div class="vh">
    <div class="vh-icon">${icon(ic)}</div>
    <div><h2>${title}</h2><p>${sub}</p></div>
  </div>`;

// opt is a radio row; rows sharing data-group form one choice, saved with
// fn(group, value) — SetOption by default.
const opt = (group, value, title, sub, sel, right = '', fn = 'SetOption') => `
  <button class="opt ${sel ? 'sel' : ''}" data-group="${group}" data-fn="${fn}" data-value="${esc(value)}">
    <span class="radio">${icon('check', '')}</span>
    <span class="row-text"><b>${title}</b>${sub ? `<small>${sub}</small>` : ''}</span>
    ${right}
  </button>`;

// sw is a switch row bound to SetFlag(key).
const sw = (key, on, title, text, disabled = false) => `
  <div class="row">
    <div class="row-text"><b>${title}</b><small>${text}</small></div>
    <label class="switch"><input type="checkbox" data-flag="${key}" ${on ? 'checked' : ''} ${disabled ? 'disabled' : ''}><span></span></label>
  </div>`;

const seg = (key, items, value) => `
  <div class="seg" data-seg="${key}">
    ${items.map(([v, t]) => `<button class="${v === value ? 'on' : ''}" data-value="${v}">${t}</button>`).join('')}
  </div>`;

// langSelect is a language dropdown saved with fn(key, value).
const langSelect = (fn, key, langs, value, empty) => `
  <select class="field" data-fn="${fn}" data-key="${key}">
    ${empty ? `<option value="">${empty}</option>` : ''}
    ${langs.map((l) => `<option value="${l.code}" ${l.code === value ? 'selected' : ''}>${esc(l.native)}</option>`).join('')}
  </select>`;

const OPTION_SAVED = {
  mode: { toggle: 'Режим: нажать и говорить', hold: 'Режим: удерживать' },
  style: { raw: 'Без обработки', clean: 'Стиль: чистка', polish: 'Стиль: редактура', formal: 'Стиль: деловой' },
};

function bindCommon() {
  panel.querySelectorAll('.opt[data-group]').forEach((el) => {
    el.addEventListener('click', async () => {
      if (el.classList.contains('sel')) return;
      const group = el.dataset.group;
      try {
        await call(el.dataset.fn, group, el.dataset.value);
      } catch { return; }
      panel.querySelectorAll(`.opt[data-group="${group}"][data-fn="${el.dataset.fn}"]`).forEach((o) => o.classList.toggle('sel', o === el));
      notify(OPTION_SAVED[group]?.[el.dataset.value] ?? 'Сохранено');
    });
  });
  panel.querySelectorAll('input[data-flag]').forEach((el) => {
    el.addEventListener('change', async () => {
      try {
        await call('SetFlag', el.dataset.flag, el.checked);
        notify('Сохранено');
      } catch {
        el.checked = !el.checked;
      }
    });
  });
  panel.querySelectorAll('.seg[data-seg]').forEach((s) => {
    s.querySelectorAll('button').forEach((b) => b.addEventListener('click', async () => {
      try {
        await call('SetOption', s.dataset.seg, b.dataset.value);
      } catch { return; }
      s.querySelectorAll('button').forEach((x) => x.classList.toggle('on', x === b));
      notify('Сохранено');
    }));
  });
  panel.querySelectorAll('select[data-fn]').forEach((el) => {
    let prev = el.value;
    el.addEventListener('change', async () => {
      try {
        await call(el.dataset.fn, el.dataset.key, el.value);
        prev = el.value;
        notify('Сохранено');
      } catch {
        el.value = prev;
      }
    });
  });
  bindHotkeyRows();
  $('#testBtn', panel)?.addEventListener('click', toggleTest);
  renderTestButton();
}

// saveOnEdit stores a text field shortly after typing stops and on blur.
function saveOnEdit(el, key) {
  let timer;
  let saved = el.value;
  const save = async () => {
    clearTimeout(timer);
    if (el.value === saved) return;
    try {
      await call('SetOption', key, el.value);
      saved = el.value;
      notify('Сохранено');
    } catch { /* reported */ }
  };
  el.addEventListener('input', () => { clearTimeout(timer); timer = setTimeout(save, 900); });
  el.addEventListener('blur', save);
}

// ---------------------------------------------------------------- hotkeys

const keycaps = (parts) => (parts?.length
  ? parts.map((p) => `<span class="kbd">${esc(p)}</span>`).join('<span class="plus">+</span>')
  : '<span class="hintline">не назначена</span>');

// hkRow is a hotkey editor for one action (data-target = its id).
function hkRow(target, title, sub, parts, { clearable = false, extra = '' } = {}) {
  return `
    <div class="hk-wrap" data-target="${esc(target)}" data-parts="${esc(JSON.stringify(parts || []))}">
      <div class="hk">
        <div class="row-text"><b>${title}</b>${sub ? `<small>${sub}</small>` : ''}</div>
        <div class="keys">${keycaps(parts)}</div>
        ${clearable && parts?.length ? `<button class="icon-btn" data-hk-clear title="Убрать сочетание">${icon('close')}</button>` : ''}
        ${extra}
        <button class="btn sm" data-hk-btn>${icon('keyboard')}${parts?.length ? 'Изменить' : 'Назначить'}</button>
      </div>
      <div class="hk-err" hidden></div>
    </div>`;
}

function bindHotkeyRows() {
  panel.querySelectorAll('.hk-wrap').forEach((w) => {
    $('[data-hk-btn]', w).addEventListener('click', () => {
      if (state.capturing) api().CancelHotkeyCapture();
      else startCapture(w);
    });
    $('[data-hk-clear]', w)?.addEventListener('click', async () => {
      await call('ClearHotkey', w.dataset.target);
      notify('Сочетание убрано');
      refresh(true);
    });
  });
}

function startCapture(w) {
  state.capturing = w.dataset.target;
  const keys = $('.keys', w);
  keys.classList.add('wait');
  keys.innerHTML = '<span class="kbd">Нажмите сочетание…</span>';
  $('[data-hk-btn]', w).innerHTML = 'Отмена';
  $('.hk-err', w).hidden = true;
  api().CaptureHotkey(w.dataset.target);
}

function onHotkeyCaptured(res) {
  state.capturing = null;
  if (res.target === 'dictate' && res.hotkey?.length) state.hotkey = res.hotkey;
  if (res.hotkey?.length) notify(`Горячая клавиша: ${res.hotkey.join(' + ')}`);
  const w = [...panel.querySelectorAll('.hk-wrap')].find((x) => x.dataset.target === res.target);
  if (!w) return;
  if (res.hotkey?.length) {
    refresh(true);
    return;
  }
  const keys = $('.keys', w);
  keys.classList.remove('wait');
  const parts = JSON.parse(w.dataset.parts || '[]');
  keys.innerHTML = keycaps(parts);
  $('[data-hk-btn]', w).innerHTML = `${icon('keyboard')}${parts.length ? 'Изменить' : 'Назначить'}`;
  const err = $('.hk-err', w);
  err.hidden = !res.error;
  err.textContent = res.error || '';
  fitWindow();
}

// ---------------------------------------------------------------- test recording

const testBlock = (source) => `
  <div>
    <div class="label"><span>Проверка</span></div>
    <div class="group">
      <div class="test">
        <button class="btn sm" id="testBtn" data-source="${source}">${icon('play')}Записать тест</button>
        <div class="lvl"><i id="testLvl"></i></div>
      </div>
      <div class="result" id="testResult" hidden></div>
    </div>
  </div>`;

function renderTestButton() {
  const b = $('#testBtn');
  if (!b) return;
  const mine = state.testing === b.dataset.source;
  const rec = mine && state.phase === 'recording';
  const busy = mine && state.phase === 'processing';
  b.classList.toggle('rec', rec);
  b.disabled = busy;
  b.innerHTML = rec ? `${icon('stop')}Остановить` : busy ? 'Распознаю…' : `${icon('play')}Записать тест`;
  if (!rec) $('#testLvl').style.width = '0';
}

async function toggleTest(e) {
  if (state.testing) {
    api().TestStop();
    return;
  }
  const source = e.currentTarget.dataset.source;
  $('#testResult').hidden = true;
  state.testing = (await api().TestStart(source)) ? source : null;
  renderTestButton();
  fitWindow();
}

function onTestLevel(level) {
  const bar = $('#testLvl');
  if (bar) bar.style.width = `${Math.round(level * 100)}%`;
}

function onTestResult(r) {
  const box = $('#testResult');
  if (!box) return;
  if (r.error) {
    box.innerHTML = `<div class="hintline err">${esc(r.error)}</div>`;
  } else {
    box.innerHTML = `
      <div><div class="lbl">Итог</div><div class="txt">${esc(r.text)}</div></div>
      ${r.raw !== r.text ? `<div><div class="lbl">Распознано</div><div class="txt raw">${esc(r.raw)}</div></div>` : ''}
      ${r.note ? `<div class="hintline err">${esc(r.note)}</div>` : ''}`;
  }
  box.hidden = false;
  fitWindow();
}

// ---------------------------------------------------------------- dictation panel

async function renderRecord() {
  const v = await api().GetRecordView();
  state.hotkey = v.hotkey;
  const def = v.mics.find((m) => m.isDefault);
  const micRows = [opt('mic', '', 'Как в Windows', def ? esc(def.name) : 'Микрофон по умолчанию', !v.micId)]
    .concat(v.mics.map((m) => opt('mic', m.id, esc(m.name), m.isDefault ? 'Сейчас выбран в Windows' : '', v.micId === m.id)));
  if (v.micId && !v.mics.some((m) => m.id === v.micId)) {
    micRows.push(opt('mic', v.micId, 'Выбранный микрофон', 'Сейчас не подключён — пока используется системный', true));
  }

  return `
    <div class="view">
      ${vh('mic', 'Диктовка', 'Горячая клавиша, режим и микрофон')}
      <div class="group">${hkRow('dictate', 'Горячая клавиша', 'Работает в любой программе', v.hotkey)}</div>
      <div>
        <div class="label"><span>Режим</span></div>
        <div class="group">
          ${opt('mode', 'toggle', 'Нажать и говорить', 'Нажмите сочетание, продиктуйте, нажмите ещё раз — текст вставится', v.mode === 'toggle')}
          ${opt('mode', 'hold', 'Удерживать', 'Говорите, пока держите клавиши; отпустили — текст вставится. Действует и на переводы', v.mode === 'hold')}
        </div>
      </div>
      <div class="note">${icon('shield')}<span>Поставьте курсор в поле ввода — например, в чат Telegram — и нажмите сочетание. Во время записи <b>Esc</b> отменяет диктовку.</span></div>
      <div>
        <div class="label"><span>Микрофон</span></div>
        <div class="group">${micRows.join('')}</div>
      </div>
      ${testBlock('mic')}
    </div>`;
}

// ---------------------------------------------------------------- system audio panel

const LISTEN_STYLES = [
  ['raw', 'Как есть', 'Текст в том виде, как его распознала модель'],
  ['clean', 'Чистка', 'Убрать слова-паразиты и повторы, расставить знаки'],
  ['polish', 'Редактура', 'Плюс абзацы, грамматика и читаемость'],
];

async function renderListen() {
  const v = await api().GetListenView();
  const def = v.devices.find((d) => d.isDefault);
  const devRows = [opt('device', '', 'Как в Windows', def ? esc(def.name) : 'Устройство вывода по умолчанию', !v.device, '', 'SetListenOption')]
    .concat(v.devices.map((d) => opt('device', d.id, esc(d.name), d.isDefault ? 'Сейчас выбрано в Windows' : '', v.device === d.id, '', 'SetListenOption')));
  if (v.device && !v.devices.some((d) => d.id === v.device)) {
    devRows.push(opt('device', v.device, 'Выбранное устройство', 'Сейчас не подключено — пока записывается системное', true, '', 'SetListenOption'));
  }
  return `
    <div class="view">
      ${vh('speaker', 'Звук системы → текст', 'Запись того, что играет в колонках или наушниках')}
      <div class="group">${hkRow('listen', 'Горячая клавиша', 'Нажали — пошла запись, нажали ещё раз — готово', v.hotkey, { clearable: true })}</div>
      <div class="note">${icon('shield')}<span>Созвон, видео, голосовое сообщение — запустите запись, а после остановки откроется окно с текстом поверх всех окон. Микрофон при этом не записывается, сигналы о начале записи не звучат.</span></div>
      <div>
        <div class="label"><span>Текст</span></div>
        <div class="group">
          ${LISTEN_STYLES.map(([id, t, s]) => opt('style', id, t, s, v.style === id, '', 'SetListenOption')).join('')}
          <div class="row">
            <div class="row-text"><b>Перевести на</b><small>Например, английское видео — сразу русским текстом</small></div>
            <div style="width:200px">${langSelect('SetListenOption', 'to', v.languages, v.to, 'Не переводить')}</div>
          </div>
        </div>
      </div>
      <div>
        <div class="label"><span>Результат</span></div>
        <div class="group">
          ${opt('output', 'show', 'Показать в окне', 'Окно с текстом откроется поверх всех окон — прочитать и скопировать', v.output === 'show', '', 'SetListenOption')}
          ${opt('output', 'clipboard', 'Скопировать в буфер обмена', 'Сразу в буфер, вставите сами, куда нужно', v.output === 'clipboard', '', 'SetListenOption')}
          ${opt('output', 'insert', 'Вставить в активное поле', 'Как при диктовке — туда, где стоит курсор', v.output === 'insert', '', 'SetListenOption')}
        </div>
      </div>
      <div>
        <div class="label"><span>Что записывать</span></div>
        <div class="group">${devRows.join('')}</div>
      </div>
      ${testBlock('system')}
    </div>`;
}

// ---------------------------------------------------------------- translation panel

async function renderTranslate() {
  const v = await api().GetTranslations();
  state.languages = v.languages;
  const name = (code) => v.languages.find((l) => l.code === code)?.native ?? code;
  const rows = v.items.map((t) => hkRow(`tr:${t.id}`,
    `${t.from ? t.from.toUpperCase() : 'Любой'} <span class="arrow">${icon('arrow')}</span> ${t.to.toUpperCase()}`,
    `${t.from ? esc(name(t.from)) : 'С любого языка'} → ${esc(name(t.to))}`, t.hotkey,
    { clearable: true, extra: `<button class="icon-btn" data-tr-del="${esc(t.id)}" title="Удалить перевод">${icon('trash')}</button>` })).join('');
  return `
    <div class="view">
      ${vh('translate', 'Перевод голосом', 'Говорите на одном языке — вставится текст на другом')}
      <div class="group">${rows || '<div class="empty">Пока нет переводов — добавьте первый ниже</div>'}</div>
      <div>
        <div class="label"><span>Новый перевод</span></div>
        <div class="group">
          <div class="row pair-row">
            ${langSelect('', 'from', v.languages, '', 'С любого языка').replace('data-fn=""', 'id="trFrom"')}
            <span class="arrow">${icon('arrow')}</span>
            ${langSelect('', 'to', v.languages, 'en').replace('data-fn=""', 'id="trTo"')}
            <button class="btn primary" id="trAdd">${icon('plus')}Добавить</button>
          </div>
        </div>
      </div>
      <div class="note">${icon('shield')}<span>Перевод берёт стиль и способ вставки из «Обработки текста», а режим «Удерживать» — из «Диктовки». На Stream Deck горячая клавиша не нужна: язык выбирается прямо в настройках кнопки «Перевод».</span></div>
    </div>`;
}

function bindTranslate() {
  $('#trAdd', panel).addEventListener('click', async () => {
    try {
      await call('AddTranslation', $('#trFrom').value, $('#trTo').value);
      notify('Перевод добавлен — назначьте ему сочетание');
      refresh(true);
    } catch { /* reported */ }
  });
  panel.querySelectorAll('[data-tr-del]').forEach((b) => b.addEventListener('click', async () => {
    await call('RemoveTranslation', b.dataset.trDel);
    notify('Перевод удалён');
    refresh(true);
  }));
}

// ---------------------------------------------------------------- result window

// A finished system-audio transcript (or translation) opens on top of
// everything so it can be read and copied; nothing goes to the clipboard by itself.
function onShowResult(r) {
  state.result = r;
  if (state.open === 'result') refresh(true);
  else openPanel('result', false);
}

async function renderResult() {
  const r = state.result;
  const ic = r.title.startsWith('Звук') ? 'speaker' : 'translate';
  return `
    <div class="view">
      ${vh(ic, esc(r.title), `${fmtLen(r.seconds)} записи · текст можно выделить или скопировать целиком`)}
      <div class="group"><div class="result-text">${esc(r.text)}</div></div>
      ${r.raw && r.raw !== r.text ? `<details class="orig"><summary>Распознанный оригинал</summary><div class="result-text raw">${esc(r.raw)}</div></details>` : ''}
      ${r.note ? `<div class="note warn">${icon('shield')}<span>${esc(r.note)}</span></div>` : ''}
      <div class="actions">
        <button class="btn" id="resClose">${icon('close')}Закрыть</button>
        <button class="btn primary" id="resCopy">${icon('copy')}Скопировать</button>
      </div>
    </div>`;
}

function bindResult() {
  $('#resCopy', panel).addEventListener('click', async (e) => {
    const b = e.currentTarget;
    try {
      await call('CopyText', state.result.text);
    } catch { return; }
    b.innerHTML = `${icon('check')}Скопировано`;
    notify('Текст в буфере обмена');
  });
  $('#resClose', panel).addEventListener('click', closeResult);
  panel.querySelector('details')?.addEventListener('toggle', fitWindow);
}

// closeResult closes the panel and, if the window was in the tray, sends it back.
async function closeResult() {
  const hide = state.result?.hideAfter;
  await closePanel();
  if (hide) hideToTray();
}

// ---------------------------------------------------------------- API panel

const DELAYS = [['minimal', 'Мин.'], ['low', 'Низкая'], ['medium', 'Средняя'], ['high', 'Высокая'], ['xhigh', 'Макс.']];
const LANGS = [['ru', 'Русский'], ['en', 'English'], ['uk', 'Українська'], ['be', 'Беларуская'], ['kk', 'Қазақша'],
  ['de', 'Deutsch'], ['fr', 'Français'], ['es', 'Español'], ['it', 'Italiano'], ['pl', 'Polski'], ['tr', 'Türkçe'], ['zh', '中文']];

async function renderAPI() {
  const v = await api().GetAPIView();
  state.hasKey = v.hasKey && !v.keyError;
  updateKeyPin();
  const keyBlock = state.hasKey ? `
    <div class="keyrow">
      ${icon('lock', 'row-icon')}
      <div class="row-text"><b>Ключ сохранён</b><small class="mono">${esc(v.keyHint)}</small></div>
      <button class="btn sm" id="keyCheck">Проверить</button>
      <button class="btn sm danger" id="keyDel">${icon('trash')}Удалить</button>
    </div>
    <div class="hintline" id="keyMsg" style="padding:0 14px 12px" hidden></div>` : `
    <div class="row col">
      <div class="row-text"><b>API-ключ OpenAI</b><small>${v.keyError ? esc(v.keyError) : 'Создайте ключ на platform.openai.com → API keys и вставьте сюда'}</small></div>
      <div class="inline">
        <input class="field mono" id="keyInput" type="password" placeholder="sk-…" autocomplete="off" spellcheck="false">
        <button class="btn primary" id="keySave">Сохранить</button>
      </div>
      <div class="hintline" id="keyMsg" hidden></div>
    </div>`;

  const langs = new Set(v.languages);
  return `
    <div class="view">
      ${vh('key', 'OpenAI API', 'Ключ, распознавание речи и сеть')}
      <div class="group">${keyBlock}</div>
      <div class="note">${icon('shield')}<span>Ключ шифруется средствами Windows (DPAPI) и привязан к вашей учётной записи: расшифровать его можно только под вашим пользователем на этом компьютере. В окно приложения он больше не выводится.</span></div>
      <div>
        <div class="label"><span>Распознавание</span><span class="count">${esc(v.model)} · $${v.perMin} за минуту</span></div>
        <div class="group">
          <div class="row col">
            <div class="row-text"><b>Точность и задержка</b><small>Чем выше, тем точнее, но дольше ждать результат. Для диктовки подходит «Высокая»</small></div>
            ${seg('delay', DELAYS, v.delay)}
          </div>
          <div class="row col">
            <div class="row-text"><b>Языки речи</b><small>Подсказка модели, можно выбрать несколько</small></div>
            <div class="chips" id="langs">${LANGS.map(([c, n]) => `<button class="chip ${langs.has(c) ? 'on' : ''}" data-lang="${c}">${n}</button>`).join('')}</div>
          </div>
          <div class="row col">
            <div class="row-text"><b>Словарь</b><small>Имена, термины и названия, по одному в строке. Помогают распознаванию и обработке</small></div>
            <textarea class="field" id="keywords" rows="3" placeholder="Например:&#10;Анна Сергеевна&#10;Kubernetes">${esc(v.keywords)}</textarea>
          </div>
        </div>
      </div>
      <div>
        <div class="label"><span>Сеть</span></div>
        <div class="group">
          <div class="row col">
            <div class="row-text"><b>Прокси</b><small>Если OpenAI недоступен напрямую: http:// или socks5://. Хранится в зашифрованном виде</small></div>
            <input class="field mono" id="proxy" placeholder="socks5://user:pass@host:1080" value="${esc(v.proxy)}" spellcheck="false" autocomplete="off">
          </div>
          <div class="row col">
            <div class="row-text"><b>Адрес API</b><small>Оставьте как есть, если не используете совместимый шлюз</small></div>
            <input class="field mono" id="baseUrl" value="${esc(v.baseUrl)}" spellcheck="false" autocomplete="off">
            <div class="inline"><span class="hintline" id="netMsg" style="flex:1"></span><button class="btn sm" id="netSave">Сохранить</button></div>
          </div>
        </div>
      </div>
    </div>`;
}

function setKeyMsg(text, kind = '') {
  const m = $('#keyMsg');
  if (!m) return;
  m.hidden = !text;
  m.className = `hintline ${kind}`;
  m.textContent = text;
  fitWindow();
}

function bindAPI() {
  const input = $('#keyInput', panel);
  if (input) {
    const save = async () => {
      const btn = $('#keySave');
      btn.disabled = true;
      btn.textContent = 'Проверка…';
      try {
        const r = await api().SaveAPIKey(input.value);
        input.value = '';
        notify(r.warning ? 'Ключ сохранён' : 'Ключ проверен и сохранён');
        await refresh(true);
        if (r.warning) setKeyMsg(r.warning, 'err');
        updateKeyPin();
      } catch (e) {
        setKeyMsg(String(e?.message ?? e), 'err');
        btn.disabled = false;
        btn.textContent = 'Сохранить';
      }
    };
    $('#keySave', panel).addEventListener('click', save);
    input.addEventListener('keydown', (e) => { if (e.key === 'Enter') save(); });
    setTimeout(() => input.focus(), 400);
  }
  $('#keyCheck', panel)?.addEventListener('click', async (e) => {
    const btn = e.currentTarget;
    btn.disabled = true;
    btn.textContent = 'Проверка…';
    try {
      setKeyMsg(await api().CheckAPI(), 'ok');
    } catch (err) {
      setKeyMsg(String(err?.message ?? err), 'err');
    }
    btn.disabled = false;
    btn.textContent = 'Проверить';
  });
  $('#keyDel', panel)?.addEventListener('click', confirmDeleteKey);

  panel.querySelectorAll('[data-lang]').forEach((chip) => chip.addEventListener('click', async () => {
    chip.classList.toggle('on');
    const langs = [...panel.querySelectorAll('[data-lang].on')].map((c) => c.dataset.lang);
    try {
      await call('SetOption', 'languages', langs.join(','));
      notify(langs.length ? 'Сохранено' : 'Язык будет определяться автоматически');
    } catch {
      chip.classList.toggle('on');
    }
  }));
  saveOnEdit($('#keywords', panel), 'keywords');

  $('#netSave', panel).addEventListener('click', async () => {
    const msg = $('#netMsg');
    try {
      await api().SetNetwork($('#baseUrl').value, $('#proxy').value);
      msg.className = 'hintline ok';
      msg.textContent = 'Сохранено';
    } catch (e) {
      msg.className = 'hintline err';
      msg.textContent = String(e?.message ?? e);
    }
  });
}

function confirmDeleteKey() {
  return openDialog('delkey', dialogHTML({
    icon: 'trash', tone: 'danger',
    title: 'Удалить API-ключ?',
    text: 'Зашифрованный ключ будет стёрт с этого компьютера. Диктовка перестанет работать, пока вы не добавите ключ снова.',
    actions: `
      <button class="btn" data-act="cancel">Отмена</button>
      <button class="btn danger" data-act="del">${icon('trash')}Удалить</button>`,
  }), (d) => {
    $('[data-act=cancel]', d).addEventListener('click', closeDialog);
    $('[data-act=del]', d).addEventListener('click', async () => {
      try {
        await call('DeleteAPIKey');
        notify('Ключ удалён');
      } catch { /* reported */ }
      await closeDialog();
      state.hasKey = false;
      updateKeyPin();
      await refresh(true);
    });
  });
}

function updateKeyPin() {
  $('#pin-api').classList.toggle('on', !state.hasKey);
}

// ---------------------------------------------------------------- text panel

const STYLES = [
  ['raw', 'Без обработки', 'Текст вставляется так, как его распознала модель'],
  ['clean', 'Чистка', 'Убрать слова-паразиты, запинки и повторы, расставить знаки. Формулировки не меняются'],
  ['polish', 'Редактура', 'Плюс грамматика, корявые фразы, абзацы и списки. Ваш стиль сохраняется'],
  ['formal', 'Деловой стиль', 'Вежливо и грамотно — для писем и рабочих чатов'],
];

const INSERTS = [
  ['paste', 'Вставить (Ctrl+V)', 'Через буфер обмена — мгновенно, с переносами строк'],
  ['type', 'Напечатать', 'Посимвольный ввод — если в программе не работает вставка'],
  ['clipboard', 'Только скопировать', 'Текст окажется в буфере обмена, вставите сами'],
];

const money = (v) => `$${Number(v).toFixed(2)}`;

async function renderText() {
  const v = await api().GetTextView();
  const models = v.models.map((m, i) => opt('model', m.id,
    `${esc(m.label)}${i === 0 ? '<span class="tag">рекомендуется</span>' : ''}`, esc(m.note), v.model === m.id,
    `<span class="price">${money(m.in)} / ${money(m.out)}<br>за 1M токенов</span>`)).join('');

  return `
    <div class="view">
      ${vh('wand', 'Обработка текста', 'Убираем воду, запинки и слова-паразиты')}
      <div class="group">${STYLES.map(([id, t, s]) => opt('style', id, t, s, v.style === id)).join('')}</div>
      <div>
        <div class="label"><span>Модель для обработки</span></div>
        <div class="group">${models}</div>
      </div>
      <div>
        <div class="label"><span>Свои правила</span></div>
        <div class="group">
          <div class="row col">
            <textarea class="field" id="customPrompt" rows="3" placeholder="Например: обращайся на «вы», пиши «ё», без смайликов, суммы в рублях с пробелами">${esc(v.customPrompt)}</textarea>
          </div>
          ${sw('noFinalDot', v.noFinalDot, 'Без точки в конце', 'Как обычно пишут в мессенджерах')}
        </div>
      </div>
      <div>
        <div class="label"><span>Вставка</span></div>
        <div class="group">${INSERTS.map(([id, t, s]) => opt('insert', id, t, s, v.insert === id)).join('')}</div>
      </div>
      <div class="group">
        ${sw('restoreClipboard', v.restoreClipboard, 'Возвращать буфер обмена', 'После вставки в буфере снова то, что было до диктовки')}
        ${sw('pressEnter', v.pressEnter, 'Сразу отправлять', 'Нажимать Enter после вставки — в Telegram сообщение уйдёт без проверки')}
      </div>
    </div>`;
}

function bindText() {
  saveOnEdit($('#customPrompt', panel), 'customPrompt');
}

// ---------------------------------------------------------------- history

const fmtLen = (s) => (s < 60 ? `${Math.round(s)} с` : `${(s / 60).toFixed(1).replace('.', ',')} мин`);

function fmtTime(iso) {
  const d = new Date(iso);
  const now = new Date();
  const time = d.toLocaleTimeString('ru-RU', { hour: '2-digit', minute: '2-digit' });
  if (d.toDateString() === now.toDateString()) return `Сегодня, ${time}`;
  return `${d.toLocaleDateString('ru-RU', { day: 'numeric', month: 'short' })}, ${time}`;
}

async function renderHistory() {
  const v = await api().GetHistory();
  state.history = v.items;
  const m = v.month;
  const rows = v.items.map((it, i) => `
    <div class="hist" data-i="${i}">
      <div class="row-text">
        <div class="hist-meta">${it.kind ? `<span class="hist-kind">${esc(it.kind)}</span>` : ''}${fmtTime(it.time)} · ${fmtLen(it.seconds)} · $${it.cost.toFixed(4)}</div>
        <div class="hist-text">${esc(it.text)}</div>
        ${it.raw !== it.text ? `<div class="hist-raw">Распознано: ${esc(it.raw)}</div>` : ''}
        ${it.note ? `<div class="hist-note">${esc(it.note)}</div>` : ''}
      </div>
      <button class="icon-btn" data-copy="${i}" title="Скопировать">${icon('copy')}</button>
    </div>`).join('');

  return `
    <div class="view">
      ${vh('history', 'История', 'Последние диктовки и расходы за месяц')}
      <div class="group stats">
        <div class="stat"><b>${m.count}</b><small>диктовок</small></div>
        <div class="stat"><b>${fmtLen(m.seconds)}</b><small>записано</small></div>
        <div class="stat"><b>$${m.cost.toFixed(m.cost < 1 ? 3 : 2)}</b><small>примерно потрачено</small></div>
      </div>
      <div>
        <div class="label"><span>Последние</span><button class="link-btn" id="histClear" ${v.items.length ? '' : 'disabled'}>${icon('trash')}Очистить</button></div>
        <div class="group">${rows || `<div class="empty">${v.keep ? 'Здесь появятся ваши диктовки' : 'Сохранение истории выключено в настройках'}</div>`}</div>
      </div>
    </div>`;
}

function bindHistory() {
  panel.querySelectorAll('.hist').forEach((el) => el.addEventListener('click', (e) => {
    if (e.target.closest('[data-copy]') || window.getSelection().toString()) return;
    el.classList.toggle('open');
    fitWindow();
  }));
  panel.querySelectorAll('[data-copy]').forEach((b) => b.addEventListener('click', async () => {
    try {
      await call('CopyText', state.history[+b.dataset.copy].text);
      notify('Скопировано');
    } catch { /* reported */ }
  }));
  $('#histClear', panel).addEventListener('click', async () => {
    await call('ClearHistory');
    notify('История очищена');
    refresh(true);
  });
}

// ---------------------------------------------------------------- settings

async function renderSettings() {
  const v = await api().GetSettings();
  const s = v.settings;
  return `
    <div class="view">
      ${vh('gear', 'Настройки', 'Запуск и поведение приложения')}
      <div class="group">
        ${sw('autorun', v.autorun, 'Автозапуск с Windows', 'Запускаться при входе в систему, чтобы горячая клавиша работала сразу')}
        ${sw('startMinimized', s.startMinimized, 'Запускать свёрнутым в трей', 'При автозапуске панель не появляется — только значок в трее')}
        ${sw('sounds', s.sounds, 'Звуковые сигналы', 'Короткий звук в начале и в конце записи')}
        ${sw('keepHistory', s.keepHistory, 'Сохранять историю', 'Последние 50 диктовок хранятся на диске в зашифрованном виде')}
        ${sw('alwaysOnTop', s.alwaysOnTop, 'Поверх всех окон', 'Панель не прячется за другими окнами')}
      </div>
      <div>
        <div class="label"><span>Stream Deck</span></div>
        <div class="group">
          <div class="row">
            ${icon('deck', 'row-icon')}
            <div class="row-text"><b>Плагин для Stream Deck</b><small>${deckText(v.deck)}</small></div>
            <button class="btn sm ${v.deck.installed ? '' : 'primary'}" id="deckInstall" ${v.deck.bundled && v.deck.software ? '' : 'disabled'}>${v.deck.installed ? 'Переустановить' : 'Установить'}</button>
          </div>
        </div>
      </div>
      <div>
        <div class="label"><span>О программе</span></div>
        <div class="group">
          <div class="about">
            <img src="img/logo-mark.png" alt="">
            <div class="row-text"><b>SpeechTo<span style="color:var(--accent)">Text</span></b><small>Версия ${esc(v.version)} · голосовой ввод через OpenAI gpt-live-transcribe</small></div>
          </div>
        </div>
      </div>
      <div class="actions">
        <button class="btn danger" id="quitBtn">${icon('close')}Выйти из приложения</button>
      </div>
    </div>`;
}

function deckText(d) {
  if (!d.software) return 'Программа Stream Deck не найдена';
  if (!d.bundled) return 'Плагин не входит в эту сборку';
  if (d.installed) return 'Установлен: кнопки «Диктовка», «Звук системы», «Перевод» и «Отмена» — в разделе SpeechToText';
  return 'Кнопки для диктовки, звука системы и переводов; во время записи кнопка горит красным';
}

function bindSettings() {
  $('#quitBtn', panel).addEventListener('click', confirmClose);
  $('#deckInstall', panel).addEventListener('click', async () => {
    try {
      await call('InstallStreamDeck');
      notify('Stream Deck предложит установить плагин — подтвердите');
    } catch { /* reported */ }
  });
}

// ---------------------------------------------------------------- bar

function onDictation(s) {
  state.live = s;
  state.phase = s.phase;
  const owner = s.source === 'system' ? '#btnListen' : s.to ? '#btnTranslate' : '#btnRecord';
  for (const id of ['#btnRecord', '#btnListen', '#btnTranslate']) {
    $(id).classList.toggle('rec', s.phase === 'recording' && id === owner);
    $(id).classList.toggle('busy', s.phase === 'processing' && id === owner);
  }
  if (s.phase === 'idle') state.testing = null;
  renderTestButton();
}

async function hideToTray() {
  shell.classList.remove('shown');
  await wait(220);
  await api().HideToTray();
  if (modal.open) {
    setModal(false);
    // The dialog grew the collapsed bar; shrink it back while hidden.
    if (modal.grewWindow) await api().Collapse();
  }
}

async function quit() {
  shell.classList.remove('shown');
  await wait(220);
  api().Quit();
}

// ---------------------------------------------------------------- dialogs

const modal = { open: false, grewWindow: false, kind: null };

function setModal(on) {
  modal.open = on;
  if (!on) modal.kind = null;
  $('#modal').classList.toggle('show', on);
  $('#modal').setAttribute('aria-hidden', String(!on));
}

// openDialog shows a dialog, first growing the window when only the bar is visible.
async function openDialog(kind, html, bind) {
  if (modal.open || state.busy) return;
  modal.kind = kind;
  $('#dialog').innerHTML = html;
  bind($('#dialog'));
  modal.grewWindow = !state.open;
  if (modal.grewWindow) {
    state.busy = true;
    const grow = api().Expand(DIALOG_H);
    setTimeout(() => setModal(true), 90);
    await grow;
    state.busy = false;
  } else {
    setModal(true);
  }
  $('#dialog .btn.primary')?.focus();
}

async function closeDialog() {
  if (!modal.open) return;
  setModal(false);
  if (modal.grewWindow) {
    await wait(200);
    await api().Collapse();
  }
}

const dialogHTML = ({ icon: ic, tone = '', title, text, actions }) => `
  <div class="dlg-icon ${tone}">${icon(ic)}</div>
  <h3 id="dlgTitle">${title}</h3>
  <p id="dlgText">${text}</p>
  <div class="dlg-actions">${actions}</div>`;

function confirmClose() {
  return openDialog('close', dialogHTML({
    icon: 'power', tone: 'danger',
    title: 'Закрыть SpeechToText?',
    text: 'Горячая клавиша перестанет работать, пока приложение закрыто. Свернуть в трей — и диктовка останется под рукой.',
    actions: `
      <button class="btn danger" data-act="quit">Да, закрыть</button>
      <button class="btn primary" data-act="tray">Нет, свернуть в трей</button>`,
  }), (d) => {
    $('[data-act=quit]', d).addEventListener('click', quit);
    $('[data-act=tray]', d).addEventListener('click', hideToTray);
  });
}

// ---------------------------------------------------------------- start

let firstAppear = true;

function appear() {
  shell.classList.remove('shown');
  void shell.offsetWidth;
  requestAnimationFrame(() => shell.classList.add('shown'));
  if (!firstAppear) return;
  firstAppear = false;
  api().GetState().then((s) => {
    state.hasKey = s.hasKey;
    state.hotkey = s.hotkey;
    updateKeyPin();
    if (!s.hasKey) {
      brandMessage('Добавьте API-ключ OpenAI');
      setTimeout(() => openPanel('api', false), 500);
    } else if (s.hotkey?.length) {
      brandMessage(`Диктовка: ${s.hotkey.join('+')}`, 4200);
    }
  }).catch(() => {});
}

function init() {
  document.querySelectorAll('[data-panel]').forEach((b) => b.addEventListener('click', () => openPanel(b.dataset.panel)));
  $('#btnMin').addEventListener('click', hideToTray);
  $('#btnClose').addEventListener('click', confirmClose);
  $('#modal').addEventListener('click', (e) => { if (e.target.id === 'modal') closeDialog(); });
  document.addEventListener('keydown', (e) => {
    if (e.key !== 'Escape') return;
    if (modal.open) closeDialog();
    else if (state.open === 'result') closeResult();
    else closePanel();
  });
  document.addEventListener('contextmenu', (e) => {
    if (!e.target.closest('input, textarea')) e.preventDefault();
  });

  const on = window.runtime.EventsOn;
  on('appear', appear);
  on('request-hide', hideToTray);
  on('notice', (msg) => notify(msg));
  on('open-panel', (name) => (name === 'close' ? confirmClose() : openPanel(name, false)));
  on('dictation', onDictation);
  on('hotkey-captured', onHotkeyCaptured);
  on('test-level', onTestLevel);
  on('test-result', onTestResult);
  on('show-result', onShowResult);
  on('history-changed', () => { if (state.open === 'history') refresh(true); });

  appear();
}

(function start() {
  if (window.go?.main?.App && window.runtime) init();
  else setTimeout(start, 20);
})();
