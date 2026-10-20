// SpeechToText for Stream Deck: keys that start dictation, system audio → text
// and voice translation in the running SpeechToText app, and light up while
// it records. The app exposes a local API (127.0.0.1, token in control.json).

import streamDeck, { SingletonAction } from '@elgato/streamdeck';
import { spawn } from 'node:child_process';
import { readFile } from 'node:fs/promises';
import path from 'node:path';
import { dataUrl, keySvg } from './keys.js';

const log = streamDeck.logger.createScope('SpeechToText');
const CONTROL = path.join(process.env.APPDATA ?? '', 'SpeechToText', 'control.json');
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

// ---------------------------------------------------------------- link to the app

const app = {
  online: false,
  state: { phase: 'idle' }, // LiveState from the app
  starting: 0,              // when we launched the app, 0 when not launching
  listeners: new Set(),
  changed() {
    for (const fn of this.listeners) fn();
  },
};

async function control() {
  return JSON.parse(await readFile(CONTROL, 'utf8'));
}

async function call(method, route, body) {
  const c = await control();
  const res = await fetch(`http://127.0.0.1:${c.port}${route}`, {
    method,
    headers: { Authorization: `Bearer ${c.token}`, 'Content-Type': 'application/json' },
    body: body && JSON.stringify(body),
    signal: AbortSignal.timeout(5000),
  });
  const data = await res.json().catch(() => ({}));
  if (!res.ok) throw new Error(data.error || `HTTP ${res.status}`);
  return data;
}

// follow keeps a Server-Sent Events stream to the app open and mirrors its
// state; it reconnects whenever the app restarts (new port and token).
async function follow() {
  for (;;) {
    try {
      const c = await control();
      const res = await fetch(`http://127.0.0.1:${c.port}/v1/events`, { headers: { Authorization: `Bearer ${c.token}` } });
      if (!res.ok) throw new Error(`HTTP ${res.status}`);
      const reader = res.body.getReader();
      const decoder = new TextDecoder();
      let buf = '';
      app.online = true;
      log.info('connected to SpeechToText');
      for (;;) {
        const { value, done } = await reader.read();
        if (done) break;
        buf += decoder.decode(value, { stream: true });
        for (let i = buf.indexOf('\n\n'); i >= 0; i = buf.indexOf('\n\n')) {
          const data = buf.slice(0, i).split('\n').find((l) => l.startsWith('data: '));
          buf = buf.slice(i + 2);
          if (data) {
            app.state = JSON.parse(data.slice(6));
            app.starting = 0;
            app.changed();
          }
        }
      }
    } catch {
      // The app is not running (yet).
    }
    if (app.online) {
      app.online = false;
      app.state = { phase: 'idle' };
      log.warn('SpeechToText closed');
      app.changed();
    }
    await sleep(1500);
  }
}

// launch starts the app. Its path is known once it has run at least once.
async function launch(args) {
  let exe;
  try {
    exe = (await control()).exe;
  } catch {
    return false;
  }
  if (!exe) return false;
  spawn(exe, args, { detached: true, stdio: 'ignore' }).unref();
  log.info(`starting ${exe} ${args.join(' ')}`);
  app.starting = Date.now();
  app.changed();
  setTimeout(() => app.changed(), 15000); // drop the "starting" look if it never came up
  return true;
}

// send runs a command. With the app closed, a toggle starts the app with the
// command on its command line, so one press is enough.
async function send(action, cmd) {
  try {
    await call('POST', '/v1/action', cmd);
    return;
  } catch (e) {
    if (!app.online) {
      const args = cmd.op === 'toggle' ? ['--toggle', cmd.action] : ['--hidden'];
      if (cmd.op === 'toggle') {
        for (const k of ['to', 'from', 'source', 'output']) if (cmd[k]) args.push(`--${k}`, cmd[k]);
      }
      if (cmd.op !== 'stop' && cmd.op !== 'cancel' && await launch(args)) return;
    }
    log.error(`${cmd.op} ${cmd.action ?? ''}: ${e.message}`);
    await action.showAlert();
  }
}

const elapsed = (since) => {
  const s = Math.max(0, Math.floor((Date.now() - (since || Date.now())) / 1000));
  return `${Math.floor(s / 60)}:${String(s % 60).padStart(2, '0')}`;
};

// ---------------------------------------------------------------- keys

class Key extends SingletonAction {
  kind = '';
  settings = new Map(); // action instance id → its settings

  onWillAppear(ev) {
    this.settings.set(ev.action.id, ev.payload.settings ?? {});
    this.paint(ev.action);
  }

  onWillDisappear(ev) {
    this.settings.delete(ev.action.id);
  }

  onDidReceiveSettings(ev) {
    this.settings.set(ev.action.id, ev.payload.settings ?? {});
    this.paint(ev.action);
  }

  paintAll() {
    for (const a of this.actions) this.paint(a);
  }

  paint(a) {
    if (!a.isKey()) return;
    const s = this.settings.get(a.id) ?? {};
    const { look, label } = this.view(s);
    a.setImage(dataUrl(keySvg(this.kind, look, label))).catch(() => {});
  }

  view(s) {
    const st = app.state;
    if (!app.online) {
      return app.starting && Date.now() - app.starting < 15000
        ? { look: 'busy', label: 'Запуск…' }
        : { look: 'off', label: this.title(s) };
    }
    if (this.mine(s, st) && st.phase === 'recording') return { look: 'rec', label: elapsed(st.since) };
    if (this.mine(s, st) && st.phase === 'processing') return { look: 'busy', label: st.to ? 'Перевод…' : 'Текст…' };
    return { look: 'idle', label: this.title(s) };
  }

  title() {
    return '';
  }

  mine() {
    return false;
  }
}

class Dictate extends Key {
  manifestId = 'com.speechtotext.deck.dictate';
  kind = 'dictate';
  title = () => 'Диктовка';
  mine = (s, st) => st.action === 'dictate';

  onKeyDown(ev) {
    const hold = ev.payload.settings?.mode === 'hold';
    return send(ev.action, { op: hold ? 'start' : 'toggle', action: 'dictate' });
  }

  onKeyUp(ev) {
    if (ev.payload.settings?.mode === 'hold') return send(ev.action, { op: 'stop' });
  }
}

class Listen extends Key {
  manifestId = 'com.speechtotext.deck.listen';
  kind = 'listen';
  title = (s) => (s.to ? `Звук → ${s.to.toUpperCase()}` : 'Звук ПК');
  mine = (s, st) => st.action === 'listen';

  onKeyDown(ev) {
    const s = ev.payload.settings ?? {};
    return send(ev.action, { op: 'toggle', action: 'listen', to: s.to || '', output: s.output || '' });
  }
}

const trSettings = (s) => ({ to: s.to || 'en', from: s.from || '', source: s.source || 'mic', output: s.output || 'insert', mode: s.mode || 'toggle' });

class Translate extends Key {
  manifestId = 'com.speechtotext.deck.translate';
  kind = 'translate';

  title = (raw) => {
    const s = trSettings(raw);
    return `${s.from ? `${s.from.toUpperCase()} ` : ''}→ ${s.to.toUpperCase()}`;
  };

  mine = (raw, st) => {
    const s = trSettings(raw);
    return (st.action === 'translate' || st.action?.startsWith('tr:')) && st.to === s.to && (st.source || 'mic') === s.source;
  };

  onKeyDown(ev) {
    const s = trSettings(ev.payload.settings ?? {});
    const hold = s.mode === 'hold' && s.source === 'mic';
    const { to, from, source, output } = s;
    return send(ev.action, { op: hold ? 'start' : 'toggle', action: 'translate', to, from, source, output });
  }

  onKeyUp(ev) {
    const s = trSettings(ev.payload.settings ?? {});
    if (s.mode === 'hold' && s.source === 'mic') return send(ev.action, { op: 'stop' });
  }
}

class Cancel extends Key {
  manifestId = 'com.speechtotext.deck.cancel';
  kind = 'cancel';

  view() {
    if (!app.online) return { look: 'off', label: 'Отмена' };
    return { look: app.state.phase === 'idle' ? 'idle' : 'armed', label: 'Отмена' };
  }

  onKeyDown(ev) {
    if (app.online) return send(ev.action, { op: 'cancel' });
  }
}

const keys = [new Dictate(), new Listen(), new Translate(), new Cancel()];
for (const k of keys) streamDeck.actions.registerAction(k);
app.listeners.add(() => keys.forEach((k) => k.paintAll()));
// Tick the timer on recording keys.
setInterval(() => {
  if (app.state.phase === 'recording') keys.forEach((k) => k.paintAll());
}, 1000);

streamDeck.connect();
follow();
