// Key and icon artwork, drawn as SVG so a key can show its state (recording,
// processing, offline) and a timer. The build writes the idle versions as the
// manifest's static images, so both always match.

const GLYPHS = {
  dictate: '<rect x="9" y="2.5" width="6" height="11.5" rx="3"/><path d="M5.5 11a6.5 6.5 0 0 0 13 0"/><path d="M12 17.5V21"/><path d="M8.5 21h7"/>',
  listen: '<path d="M4 9.5h3.2L12 5.5v13l-4.8-4H4a1 1 0 0 1-1-1v-3a1 1 0 0 1 1-1Z"/><path d="M15.5 9a4 4 0 0 1 0 6"/><path d="M18.2 6.3a8 8 0 0 1 0 11.4"/>',
  translate: '<path d="M3.5 5.5h9"/><path d="M8 3.5v2"/><path d="M10.5 5.5c-.8 3.7-3.2 6.6-6.5 8"/><path d="M5.5 8.5c1 2 2.8 3.8 5 4.8"/><path d="m12.5 20.5 4-9.5 4 9.5"/><path d="M13.8 17.5h5.4"/>',
  cancel: '<circle cx="12" cy="12" r="9"/><path d="m9 9 6 6M15 9l-6 6"/>',
};

// Colours per look: background, glyph, label.
const LOOKS = {
  idle: ['#1c1c22', '#7fb0fa', '#e4e4e7'],
  armed: ['#3a1f22', '#fca5a5', '#fecaca'], // cancel key while something runs
  rec: ['#dc2626', '#ffffff', '#ffffff'],
  busy: ['#2563eb', '#ffffff', '#ffffff'],
  off: ['#202024', '#55555f', '#6b6b76'],
};

const esc = (s) => String(s).replace(/[&<>"]/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;' }[c]));

/**
 * keySvg draws a 144×144 key.
 * @param {keyof GLYPHS} kind
 * @param {keyof LOOKS} look
 * @param {string} label   bottom line, e.g. "Диктовка", "0:07", "RU → EN"
 */
export function keySvg(kind, look, label) {
  const [bg, fg, text] = LOOKS[look] ?? LOOKS.idle;
  const scale = 2.6;
  const top = label ? 26 : 41;
  const dot = look === 'rec' ? '<circle cx="124" cy="20" r="8" fill="#fff"/>' : '';
  const size = label.length > 7 ? 20 : 23;
  return `<svg xmlns="http://www.w3.org/2000/svg" width="144" height="144" viewBox="0 0 144 144">`
    + `<rect width="144" height="144" fill="${bg}"/>${dot}`
    + `<g transform="translate(${72 - 12 * scale} ${top}) scale(${scale})" fill="none" stroke="${fg}" stroke-width="1.7" stroke-linecap="round" stroke-linejoin="round">${GLYPHS[kind]}</g>`
    + (label ? `<text x="72" y="124" text-anchor="middle" font-family="Segoe UI, Arial, sans-serif" font-size="${size}" font-weight="600" fill="${text}">${esc(label)}</text>` : '')
    + '</svg>';
}

/** iconSvg is the white glyph shown in the Stream Deck action list. */
export function iconSvg(kind) {
  return `<svg xmlns="http://www.w3.org/2000/svg" width="20" height="20" viewBox="0 0 24 24" fill="none" stroke="#fff" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round">${GLYPHS[kind]}</svg>`;
}

/** dataUrl turns an SVG into something setImage accepts. */
export const dataUrl = (svg) => `data:image/svg+xml;charset=utf8,${encodeURIComponent(svg)}`;

export const KINDS = Object.keys(GLYPHS);
