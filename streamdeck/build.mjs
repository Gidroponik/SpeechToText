// Builds the Stream Deck plugin: bundles src/ into bin/plugin.js, draws the
// static key and action images, validates and packs the plugin into
// ../deck/, where the app embeds it for one-click install.
//
//   npm run build

import { build } from 'esbuild';
import { execFileSync } from 'node:child_process';
import { mkdir, rm, writeFile } from 'node:fs/promises';
import { iconSvg, keySvg, KINDS } from './src/keys.js';

const PLUGIN = 'com.speechtotext.deck.sdPlugin';
const LABELS = { dictate: 'Диктовка', listen: 'Звук ПК', translate: '→ EN', cancel: 'Отмена' };

await build({
  entryPoints: ['src/plugin.js'],
  bundle: true,
  platform: 'node',
  target: 'node20',
  format: 'esm',
  outfile: `${PLUGIN}/bin/plugin.js`,
  banner: { js: "import { createRequire } from 'node:module'; const require = createRequire(import.meta.url);" },
  logLevel: 'warning',
});

for (const kind of KINDS) {
  const dir = `${PLUGIN}/imgs/actions/${kind}`;
  await mkdir(dir, { recursive: true });
  await writeFile(`${dir}/key.svg`, keySvg(kind, 'idle', LABELS[kind]));
  await writeFile(`${dir}/icon.svg`, iconSvg(kind));
}
await writeFile(`${PLUGIN}/imgs/plugin/category-icon.svg`, iconSvg('dictate').replace('width="20" height="20"', 'width="28" height="28"'));

await rm(`${PLUGIN}/logs`, { recursive: true, force: true }); // left by test runs
const cli = (...args) => execFileSync(process.execPath, ['node_modules/@elgato/cli/bin/streamdeck.mjs', ...args], { stdio: 'inherit' });
cli('validate', PLUGIN, '--no-update-check');
await mkdir('../deck', { recursive: true });
cli('pack', PLUGIN, '--output', '../deck', '--force', '--no-update-check', '--no-file-list');
