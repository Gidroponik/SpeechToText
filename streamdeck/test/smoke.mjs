// Smoke test of the built plugin (npm test): runs bin/plugin.js the way
// Stream Deck does, against a fake Stream Deck (WebSocket) and a fake
// SpeechToText control API, and checks the whole loop:
// register → key appears → press → command reaches the app → app state → key turns red.

import { spawn } from 'node:child_process';
import { mkdtempSync, mkdirSync, writeFileSync } from 'node:fs';
import http from 'node:http';
import { tmpdir } from 'node:os';
import path from 'node:path';
import { WebSocketServer } from 'ws';

const PLUGIN = 'com.speechtotext.deck.sdPlugin';
const fail = (msg) => {
  console.error(`FAIL: ${msg}`);
  process.exit(1);
};
setTimeout(() => fail('timeout'), 20000).unref();

// Fake SpeechToText: control.json in a temporary %APPDATA%, SSE and actions.
const appdata = mkdtempSync(path.join(tmpdir(), 'stt-smoke-'));
const commands = [];
let events;
const api = http.createServer((req, res) => {
  if (req.headers.authorization !== 'Bearer t0ken') return res.writeHead(401).end();
  if (req.url === '/v1/events') {
    res.writeHead(200, { 'Content-Type': 'text/event-stream' });
    res.write('data: {"phase":"idle"}\n\n');
    events = res;
    return;
  }
  if (req.url === '/v1/action' && req.method === 'POST') {
    let body = '';
    req.on('data', (c) => { body += c; });
    req.on('end', () => {
      commands.push(JSON.parse(body));
      res.writeHead(200, { 'Content-Type': 'application/json' }).end('{"phase":"idle"}');
    });
    return;
  }
  res.writeHead(404).end();
});
await new Promise((r) => api.listen(0, '127.0.0.1', r));
mkdirSync(path.join(appdata, 'SpeechToText'));
writeFileSync(path.join(appdata, 'SpeechToText', 'control.json'), JSON.stringify({ port: api.address().port, token: 't0ken', exe: '' }));

// Fake Stream Deck.
const wss = new WebSocketServer({ port: 0, host: '127.0.0.1' });
await new Promise((r) => wss.on('listening', r));
const info = {
  application: { font: 'Segoe UI', language: 'ru', platform: 'windows', platformVersion: '10.0.26200', version: '7.3.1.22604' },
  plugin: { uuid: 'com.speechtotext.deck', version: '1.1.0.0' },
  devicePixelRatio: 2,
  colors: {},
  devices: [{ id: 'dev1', name: 'Stream Deck', size: { columns: 5, rows: 3 }, type: 0 }],
};
const child = spawn(process.execPath, ['bin/plugin.js', '-port', String(wss.address().port), '-pluginUUID', 'PLUGIN', '-registerEvent', 'registerPlugin', '-info', JSON.stringify(info)], {
  cwd: PLUGIN, // Stream Deck starts plugins in their folder
  env: { ...process.env, APPDATA: appdata },
  stdio: ['ignore', 'inherit', 'inherit'],
});
child.on('exit', (code) => code && fail(`plugin exited with ${code}`));

const sock = await new Promise((r) => wss.on('connection', r));
const inbox = [];
let waiter;
sock.on('message', (m) => {
  inbox.push(JSON.parse(m));
  waiter?.();
});
const expect = async (what, pred) => {
  for (;;) {
    const i = inbox.findIndex(pred);
    if (i >= 0) return inbox.splice(i, 1)[0];
    await new Promise((r) => { waiter = r; });
  }
};
const svgOf = (msg) => decodeURIComponent(msg.payload.image.split(',')[1]);

await expect('registration', (m) => m.event === 'registerPlugin' && m.uuid === 'PLUGIN');
console.log('ok  registered');

const ctx = 'KEY1';
const base = { action: 'com.speechtotext.deck.dictate', context: ctx, device: 'dev1' };
sock.send(JSON.stringify({ ...base, event: 'willAppear', payload: { controller: 'Keypad', coordinates: { column: 0, row: 0 }, isInMultiAction: false, settings: {} } }));
let img = await expect('idle image', (m) => m.event === 'setImage' && m.context === ctx);
if (!svgOf(img).includes('Диктовка')) fail('idle key lacks its label');
console.log('ok  key painted');

sock.send(JSON.stringify({ ...base, event: 'keyDown', payload: { coordinates: { column: 0, row: 0 }, isInMultiAction: false, settings: {} } }));
for (let i = 0; i < 50 && !commands.length; i++) await new Promise((r) => setTimeout(r, 50));
if (commands[0]?.op !== 'toggle' || commands[0]?.action !== 'dictate') fail(`command ${JSON.stringify(commands[0])}`);
console.log('ok  key press → toggle dictate');

events.write(`data: ${JSON.stringify({ phase: 'recording', action: 'dictate', source: 'mic', since: Date.now() - 7000 })}\n\n`);
img = await expect('recording image', (m) => m.event === 'setImage' && m.context === ctx && svgOf(m).includes('#dc2626'));
if (!svgOf(img).includes('0:07')) fail('recording key lacks the timer');
console.log('ok  recording key is red with a timer');

events.write('data: {"phase":"idle"}\n\n');
await expect('idle again', (m) => m.event === 'setImage' && m.context === ctx && svgOf(m).includes('Диктовка'));
console.log('ok  back to idle');

console.log('PASS');
child.kill();
events.end();
api.close();
wss.close();
process.exit(0);
