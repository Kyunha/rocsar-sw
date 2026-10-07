/* ROCSAR Ground Station — instrument widgets.
 *
 * Pure string builders: no DOM, no state, no side effects. They return SVG or
 * HTML that the panels drop in with innerHTML, which is the idiom the console
 * already uses. Keeping them pure is what makes them testable without a browser
 * (widgets.test.ts, run by `npm test`) and what lets the build render them to a
 * PNG for a visual check.
 *
 * Three rules from GUI_ARCHITECTURE.md are enforced here rather than at the
 * call sites, because a call site is the thing that gets forgotten:
 *
 *   7.1  absence is not zero. A gauge with no value renders an explicit empty
 *        state; it never renders a zero-length bar that reads as "0".
 *   7.2  a held reading is not a measurement. barGauge(null, ...) renders the
 *        hatched held state and the word "held", never a number.
 *   9.4  no unit conversion. Nothing here converts ticks to degrees, Hz to kHz,
 *        or bytes to bits. The servo ring is drawn on the 0-4095 tick range the
 *        wire and internal/client/commands.go already use, and degrees are shown
 *        as the number the view supplied.
 *
 * Colours are carried by class, not inline style, so the palette stays in
 * style.css with the rest of the console. Every state that has a colour also has
 * its word (5): nothing here is distinguishable by colour alone.
 */

/* Local escape. main.ts has its own esc(); this module is imported by it, so a
 * shared helper would have to move to a third file to avoid a cycle. Three
 * replacements are cheaper than that. */
function escHtml(s: string): string {
    return s.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;');
}

function escAttr(s: string): string {
    return escHtml(s).replace(/"/g, '&quot;');
}

/* Numbers into SVG coordinates. Two decimals is sub-pixel at any size used
 * here, and keeps the markup small enough to read when debugging. */
function f(v: number): string {
    return v.toFixed(2);
}

/* ------------------------------------------------------------------ */
/* geometry and scalars                                               */
/* ------------------------------------------------------------------ */

export const TICK_MIN = 0;
export const TICK_MAX = 4095;

/** shortestDelta is the signed shortest angular difference
 *  `target - current`, in (-180, 180]. `+180` rather than `-180` for the
 *  antipodal case because there is no shortest direction; the compass says so
 *  in its label rather than picking one silently. */
export function shortestDelta(currentDeg: number, targetDeg: number): number {
    let d = (targetDeg - currentDeg) % 360;
    if (d > 180) {
        d -= 360;
    } else if (d <= -180) {
        d += 360;
    }
    return d;
}

export function clamp(v: number, lo: number, hi: number): number {
    return v < lo ? lo : v > hi ? hi : v;
}

/** percent maps value/max to 0..100, clamped. A non-positive or non-finite max
 *  yields 0 rather than NaN, so a malformed frame cannot draw a broken bar. */
export function percent(value: number, max: number): number {
    if (!Number.isFinite(value) || !Number.isFinite(max) || max <= 0) {
        return 0;
    }
    return clamp((value / max) * 100, 0, 100);
}

/** tickAngle maps a servo tick on the 0..4095 range to a dial angle in degrees,
 *  with 0 at the top. This is a dial position, not a mechanical angle: the wire
 *  reports the mechanical angle separately as current_angle_deg and that number
 *  is displayed as given (9.4). */
export function tickAngle(tick: number): number {
    return (percent(clamp(tick, TICK_MIN, TICK_MAX) - TICK_MIN, TICK_MAX - TICK_MIN) / 100) * 360;
}

/* polar converts a dial angle (0 = up/north, increasing clockwise) to an SVG
 * point. SVG's y axis grows downward, so the sine term is x and the negated
 * cosine term is y. */
function polar(cx: number, cy: number, r: number, deg: number): [number, number] {
    const rad = (deg * Math.PI) / 180;
    return [cx + r * Math.sin(rad), cy - r * Math.cos(rad)];
}

/** clockText renders a wall-clock instant `seconds` from `now` as HH:MM, or
 *  '-' when the time is unknown. A duration is not a plan; an operator waiting
 *  an hour wants to know when, not how many seconds. */
export function clockText(seconds: number, now: Date = new Date()): string {
    if (!Number.isFinite(seconds) || seconds < 0) {
        return '-';
    }
    const t = new Date(now.getTime() + seconds * 1000);
    return `${t.getHours().toString().padStart(2, '0')}:${t.getMinutes().toString().padStart(2, '0')}`;
}

/** durationText renders a span as m:ss, or h:mm:ss past an hour. */
export function durationText(seconds: number): string {
    if (!Number.isFinite(seconds) || seconds < 0) {
        return '-';
    }
    const s = Math.round(seconds);
    const h = Math.floor(s / 3600);
    const m = Math.floor((s % 3600) / 60);
    const sec = s % 60;
    if (h > 0) {
        return `${h}:${m.toString().padStart(2, '0')}:${sec.toString().padStart(2, '0')}`;
    }
    return `${m}:${sec.toString().padStart(2, '0')}`;
}

/** Ring is a bounded append-only history for the sparklines. Telemetry is 1 Hz,
 *  so the cap is a duration: 120 samples is two minutes. Bounded on purpose --
 *  an unbounded buffer in a window left open overnight is a leak. */
export class Ring {
    private readonly buf: number[] = [];
    private readonly cap: number;

    public constructor(cap: number) {
        this.cap = cap;
    }

    public push(v: number): void {
        if (!Number.isFinite(v)) {
            return;
        }
        this.buf.push(v);
        if (this.buf.length > this.cap) {
            this.buf.shift();
        }
    }

    public values(): number[] {
        return this.buf.slice();
    }

    public get length(): number {
        return this.buf.length;
    }
}

/* ------------------------------------------------------------------ */
/* compass                                                            */
/* ------------------------------------------------------------------ */

export interface CompassOpts {
    currentDeg: number;
    targetDeg: number;
    /** the heading is the flight controller's held value, not a measurement */
    held?: boolean;
    size?: number;
    deadbandDeg?: number;
}

/** compass draws the gondola heading against its target. The needle colours
 *  match the map's bearing lines (#22d3ee current, #ffb454 target) so the rose
 *  and the map are read as one instrument. */
export function compass(o: CompassOpts): string {
    const size = o.size ?? 172;
    const cx = size / 2;
    const cy = size / 2;
    const rRose = size / 2 - 16;
    const rNeedle = rRose - 14;
    const rArc = rRose - 5;
    const err = shortestDelta(o.currentDeg, o.targetDeg);
    const dead = o.deadbandDeg ?? 0.5;
    const onTarget = Math.abs(err) <= dead;

    const marks: string[] = [];
    for (let d = 0; d < 360; d += 15) {
        const cardinal = d % 90 === 0;
        const mid = d % 45 === 0;
        const [x1, y1] = polar(cx, cy, rRose, d);
        const [x2, y2] = polar(cx, cy, rRose - (cardinal ? 9 : mid ? 7 : 4), d);
        marks.push(
            `<line class="${cardinal ? 'w-tick w-tick-major' : 'w-tick'}" x1="${f(x1)}" y1="${f(y1)}" x2="${f(x2)}" y2="${f(y2)}"/>`,
        );
    }

    const cards: string[] = [];
    const cardinals: ReadonlyArray<readonly [number, string]> = [
        [0, 'N'],
        [90, 'E'],
        [180, 'S'],
        [270, 'W'],
    ];
    for (const [d, label] of cardinals) {
        const [x, y] = polar(cx, cy, rRose - 20, d);
        cards.push(
            `<text class="w-card" x="${f(x)}" y="${f(y)}" text-anchor="middle" dominant-baseline="central">${label}</text>`,
        );
    }

    let arc = '';
    if (!onTarget) {
        const large = Math.abs(err) > 180 ? 1 : 0;
        const sweep = err > 0 ? 1 : 0;
        const [ax, ay] = polar(cx, cy, rArc, o.currentDeg);
        const [bx, by] = polar(cx, cy, rArc, o.currentDeg + err);
        arc = `<path class="w-arc" d="M ${f(ax)} ${f(ay)} A ${f(rArc)} ${f(rArc)} 0 ${large} ${sweep} ${f(bx)} ${f(by)}"/>`;
    }

    const [hx, hy] = polar(cx, cy, rNeedle, o.currentDeg);
    const [hxb, hyb] = polar(cx, cy, -12, o.currentDeg);
    const [tx, ty] = polar(cx, cy, rNeedle, o.targetDeg);
    const [txb, tyb] = polar(cx, cy, -12, o.targetDeg);

    const held = o.held === true;
    const errText = onTarget ? 'on target' : `${err > 0 ? '+' : '-'}${Math.abs(err).toFixed(1)}°`;
    const errKind = onTarget ? 'w-ok' : Math.abs(err) > 10 ? 'w-bad' : 'w-warn';

    return `<div class="w-compass">
  <svg viewBox="0 0 ${size} ${size}" width="${size}" height="${size}" role="img"
       aria-label="heading ${o.currentDeg.toFixed(1)} degrees, target ${o.targetDeg.toFixed(1)} degrees, error ${errText}">
    <circle class="w-rose" cx="${f(cx)}" cy="${f(cy)}" r="${f(rRose)}"/>
    ${marks.join('')}
    ${cards.join('')}
    ${arc}
    <line class="w-needle-target" x1="${f(txb)}" y1="${f(tyb)}" x2="${f(tx)}" y2="${f(ty)}"/>
    <line class="w-needle-current${held ? ' w-held' : ''}" x1="${f(hxb)}" y1="${f(hyb)}" x2="${f(hx)}" y2="${f(hy)}"/>
    <circle class="w-hub" cx="${f(cx)}" cy="${f(cy)}" r="3.5"/>
  </svg>
  <div class="w-legend">
    <span class="w-current">&#9679; ${o.currentDeg.toFixed(1)}°${held ? ' <i>(held)</i>' : ''}</span>
    <span class="w-target">&#9679; target ${o.targetDeg.toFixed(1)}°</span>
    <span class="${errKind}" title="shortest angular difference; ±180° is ambiguous">${errText}</span>
  </div>
</div>`;
}

/* ------------------------------------------------------------------ */
/* servo tick ring                                                    */
/* ------------------------------------------------------------------ */

/** tickRing draws a servo's position on its full 0..4095 range. The range is
 *  the servo's own mechanical range -- TICKS_PER_DEGREE is 4096/360, so 4095
 *  ticks is a full turn -- which is why a ring is the honest shape and why no
 *  scaling decision is being made here. */
export function tickRing(tick: number, size = 54): string {
    const cx = size / 2;
    const cy = size / 2;
    const r = size / 2 - 6;
    const marks: string[] = [];
    for (let d = 0; d < 360; d += 30) {
        const [x1, y1] = polar(cx, cy, r, d);
        const [x2, y2] = polar(cx, cy, r - 4, d);
        marks.push(`<line class="w-tick" x1="${f(x1)}" y1="${f(y1)}" x2="${f(x2)}" y2="${f(y2)}"/>`);
    }
    const a = tickAngle(tick);
    const [nx, ny] = polar(cx, cy, r - 2, a);
    const [bx, by] = polar(cx, cy, -4, a);
    return `<svg class="w-ring" viewBox="0 0 ${size} ${size}" width="${size}" height="${size}" role="img"
      aria-label="servo position tick ${tick} of ${TICK_MAX}">
      <circle class="w-rose" cx="${f(cx)}" cy="${f(cy)}" r="${f(r)}"/>
      ${marks.join('')}
      <line class="w-needle-current" x1="${f(bx)}" y1="${f(by)}" x2="${f(nx)}" y2="${f(ny)}"/>
      <circle class="w-hub" cx="${f(cx)}" cy="${f(cy)}" r="2.2"/>
    </svg>`;
}

/* ------------------------------------------------------------------ */
/* bars, chips, tiles, sparklines                                     */
/* ------------------------------------------------------------------ */

export interface BarOpts {
    label: string;
    /** null means held or absent. Never drawn as a zero-length bar. */
    value: number | null;
    max: number;
    unit: string;
    absentText?: string;
    warnAt?: number;
    badAt?: number;
}

export function barGauge(o: BarOpts): string {
    if (o.value === null) {
        const word = o.absentText ?? 'held';
        return `<div class="w-bar">
      <span class="w-bar-label">${escHtml(o.label)}</span>
      <span class="w-bar-track w-bar-held" role="img" aria-label="${escAttr(o.label)} ${escAttr(word)}"><span class="w-hatch"></span></span>
      <span class="w-bar-value w-held-word">${escHtml(word)}</span>
    </div>`;
    }
    const p = percent(o.value, o.max);
    const kind =
        o.badAt !== undefined && o.value >= o.badAt ? 'w-bad'
        : o.warnAt !== undefined && o.value >= o.warnAt ? 'w-warn'
        : '';
    return `<div class="w-bar">
      <span class="w-bar-label">${escHtml(o.label)}</span>
      <span class="w-bar-track" role="img" aria-label="${escAttr(o.label)} ${o.value} ${escAttr(o.unit)}">
        <span class="w-bar-fill ${kind}" style="width:${p.toFixed(1)}%"></span>
      </span>
      <span class="w-bar-value">${o.value}${escHtml(o.unit)}</span>
    </div>`;
}

export type ChipKind = 'ok' | 'bad' | 'warn' | 'info' | 'dim';

export function stateChip(text: string, kind: ChipKind = 'dim', title?: string): string {
    const t = title === undefined ? '' : ` title="${escAttr(title)}"`;
    return `<span class="w-chip w-${kind}"${t}>${escHtml(text)}</span>`;
}

export interface TileOpts {
    label: string;
    value: string;
    sub?: string;
    kind?: ChipKind;
}

export function statTile(o: TileOpts): string {
    const cls = o.kind === undefined ? '' : ` w-${o.kind}`;
    const sub = o.sub === undefined ? '' : `<div class="w-tile-sub">${escHtml(o.sub)}</div>`;
    return `<div class="w-tile${cls}">
      <div class="w-tile-label">${escHtml(o.label)}</div>
      <div class="w-tile-value">${escHtml(o.value)}</div>
      ${sub}
    </div>`;
}

export interface SparkOpts {
    values: number[];
    label: string;
    width?: number;
    height?: number;
    /** floor for the y range, so a flat-zero series is not amplified to noise */
    min?: number;
}

export function sparkline(o: SparkOpts): string {
    const w = o.width ?? 132;
    const h = o.height ?? 26;
    const vs = o.values;
    if (vs.length < 2) {
        return `<svg class="w-spark" width="${w}" height="${h}" viewBox="0 0 ${w} ${h}" role="img"
          aria-label="${escAttr(o.label)}: no history yet"><text class="w-spark-empty" x="2" y="${h - 7}">no history</text></svg>`;
    }
    let lo = o.min === undefined ? Math.min(...vs) : Math.min(o.min, Math.min(...vs));
    let hi = Math.max(...vs);
    if (hi - lo < 1e-9) {
        hi = lo + 1;
    }
    const stepX = w / (vs.length - 1);
    const pts = vs
        .map((v, i) => `${f(i * stepX)},${f(h - ((v - lo) / (hi - lo)) * (h - 4) - 2)}`)
        .join(' ');
    return `<svg class="w-spark" width="${w}" height="${h}" viewBox="0 0 ${w} ${h}" role="img"
      aria-label="${escAttr(o.label)}"><polyline class="w-spark-line" points="${pts}"/></svg>`;
}

/* ------------------------------------------------------------------ */
/* knob                                                               */
/* ------------------------------------------------------------------ */

export interface KnobOpts {
    /** id of the numeric input this knob mirrors; the input stays the source
     *  of truth so numArg() and the commit buttons are unchanged. */
    forId: string;
    label: string;
    min: number;
    max: number;
    step: number;
    value: number;
    unit: string;
    size?: number;
}

/** knobGeometry returns the four needle coordinates for a value. Exported so
 *  the live console can repaint the needle while an input is typed into without
 *  re-rendering the SVG and dropping its listeners. */
export function knobGeometry(
    value: number,
    min: number,
    max: number,
    size = 64,
): { x1: number; y1: number; x2: number; y2: number } {
    const cx = size / 2;
    const cy = size / 2;
    const r = size / 2 - 8;
    const frac = max > min ? clamp((value - min) / (max - min), 0, 1) : 0;
    const a = -135 + frac * 270;
    const [nx, ny] = polar(cx, cy, r - 3, a);
    const [bx, by] = polar(cx, cy, -3, a);
    return { x1: bx, y1: by, x2: nx, y2: ny };
}

export function knob(o: KnobOpts): string {
    const size = o.size ?? 64;
    const cx = size / 2;
    const cy = size / 2;
    const r = size / 2 - 8;
    const g = knobGeometry(o.value, o.min, o.max, size);
    const [sx, sy] = polar(cx, cy, r, -135);
    const [ex, ey] = polar(cx, cy, r, 135);
    return `<svg class="w-knob" data-knob-for="${escAttr(o.forId)}"
      data-min="${o.min}" data-max="${o.max}" data-step="${o.step}" data-size="${size}"
      viewBox="0 0 ${size} ${size}" width="${size}" height="${size}"
      role="slider" tabindex="0" aria-label="${escAttr(o.label)}"
      aria-valuemin="${o.min}" aria-valuemax="${o.max}" aria-valuenow="${o.value}"
      aria-valuetext="${o.value} ${escAttr(o.unit)}">
      <path class="w-knob-arc" d="M ${f(sx)} ${f(sy)} A ${f(r)} ${f(r)} 0 1 1 ${f(ex)} ${f(ey)}"/>
      <line class="w-knob-needle" x1="${f(g.x1)}" y1="${f(g.y1)}" x2="${f(g.x2)}" y2="${f(g.y2)}"/>
      <circle class="w-knob-hub" cx="${f(cx)}" cy="${f(cy)}" r="3"/>
    </svg>`;
}

/** knobValueFromPointer maps a pointer offset from the knob centre to a snapped
 *  value on [min, max]. `dx` is rightward and `dy` is downward, matching
 *  clientX/clientY arithmetic. Angles outside the -135..135 sweep are clamped to
 *  the nearest end, so the bottom dead-zone cannot wrap around. */
export function knobValueFromPointer(
    min: number,
    max: number,
    step: number,
    dx: number,
    dy: number,
): number {
    const angle = clamp((Math.atan2(dx, -dy) * 180) / Math.PI, -135, 135);
    const frac = (angle + 135) / 270;
    const raw = min + frac * (max - min);
    const snapped = Math.round(raw / step) * step;
    return clamp(Number(snapped.toFixed(6)), min, max);
}
