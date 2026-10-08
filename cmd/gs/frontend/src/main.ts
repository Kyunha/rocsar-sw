/* ROCSAR Ground Station.
 *
 * Vanilla TypeScript, no framework. The window renders what Go decodes: every
 * value arrives through the WebSocket bridge (first paint) and events
 * (everything after), already shaped by internal/gsview. This file formats and
 * places; it never converts units and never invents a zero. The panel renderers
 * live in panels.ts and the instruments in widgets.ts, both pure, so the same
 * markup can be rendered from fixtures without a console.
 *
 * Motion commands fire without a confirmation step (GUI_ARCHITECTURE.md 10). The
 * knobs beside the motion inputs set values; the buttons commit them. Nothing is
 * sent while a knob is dragged.
 *
 * The console is a pure web app: the Go binary serves this file and the operator
 * opens the URL in their own browser. There is no generated binding layer --
 * bridge.ts is the hand-written client and models.ts the hand-written types.
 * See GUI_ARCHITECTURE.md sections 1.2-1.4.
 */
import 'maplibre-gl/dist/maplibre-gl.css';
import { bearingTo, initMap, setMapClickHandler } from './map';
import './style.css';
import { call, on, connect } from './bridge';
import {
    esc,
    fmtBytes,
    renderAttention,
    renderBudgetTiles,
    renderCameraSdrLink,
    renderDownload,
    renderGnss,
    renderLinkLine,
    renderPico,
    renderSystem,
    telemetryAgeRing,
} from './panels';
import { Ring, knob, knobGeometry, knobValueFromPointer } from './widgets';
import type { View, CommandResult, LinkState, Config, Entry, SdrParamsPatch, FrameEvent, DownloadProgress, DownloadDone } from './models';

/* Stale after three telemetry intervals. A constant, not configuration --
 * configuration arrives in step 7 ([gui] stale_after) and replaces this with
 * a value from the console's own config. Until then the number lives here,
 * in one place, named. */
const STALE_AFTER_S = 3;

type Cmd = CommandResult;

/* Staleness is measured on the laptop's own clock, by the laptop's own
 * arithmetic, and never by comparing two clocks.
 *
 * It used to be done the other way: parse the OBC's `generated_at` -- the Pi's
 * wall clock -- and subtract it from Date.now(). Those are different machines,
 * and the failure modes are not symmetric. A laptop running fast marks every
 * frame instantly stale and disables motion forever with nothing on screen to
 * explain why; a laptop running slow reads a link that died an hour ago as
 * fresh, which is the fail-open direction on the one rule this project calls a
 * safety property. A malformed timestamp made Date.parse return NaN, and NaN
 * comparisons are false, so it read fresh too -- silently, permanently.
 *
 * `last_frame_age_s` is the right number and was already here: cmd/gs/app.go
 * converts it from a duration that internal/client measured with time.Since
 * against a time.Now() taken on this machine when the frame arrived. Both ends
 * of the subtraction are ours, so there is nothing to disagree with.
 *
 * `frames_received` is the companion, because age alone cannot tell "a frame
 * arrived just now" from "no frame has ever arrived" -- ageSince reports zero
 * for both. Zero frames means we are looking at a cached picture of a vehicle we
 * have never reached, which is more stale than any age, not less. */
let frameAgeS = 0;
let framesSeen = 0;
let currentPath = '';

/* The last decoded frame and the last link state. The attention summary is a
 * function of both and they arrive on different events, so whichever lands
 * second redraws it. Deriving either from the DOM would mean reading rendered
 * text back, which is the round trip that breaks silently. */
let currentView: View | null = null;
let lastLink: LinkState | null = null;

/* Frame-age history for the link sparkline: two minutes at 1 Hz. Bounded,
 * because a window left open overnight is not. */
const ageHistory = telemetryAgeRing();

/* Measured tx history for the link-budget sparkline, same bound and the same
 * reason. Only real measurements are pushed: an absent reading must not put a
 * 0 into the history, or the sparkline would draw an idle link where the truth
 * is "unknown". */
const txHistory = new Ring(120);

function req(id: string): HTMLElement {
    const el = document.getElementById(id);
    if (el === null) {
        throw new Error(`missing element #${id}`);
    }
    return el;
}

function logLine(text: string): void {
    const log = req('log');
    const el = document.createElement('div');
    el.textContent = text;
    log.appendChild(el);
    while (log.children.length > 200) {
        log.removeChild(log.children[0]);
    }
    log.scrollTop = log.scrollHeight;
}

function cmdLine(name: string, r: Cmd): void {
    const el = req('cmd-result');
    el.textContent = r.success
        ? `${name}: ok — ${r.message}`
        : `${name}: FAILED (${r.error}) ${r.message}`;
    el.classList.toggle('err', !r.success);
    logLine(`${name}: success=${r.success} error=${r.error} ${r.message}`);
}

async function run(name: string, fn: () => Promise<Cmd>): Promise<void> {
    try {
        cmdLine(name, await fn());
    } catch (err) {
        cmdLine(name, { request_id: '', success: false, error: 'ERROR_INTERNAL', message: String(err), artefact: null });
    }
}

/* Motion commands fire directly, with no confirmation step.
 *
 * GUI_ARCHITECTURE.md 10 previously required a two-step gate on motion, on the
 * argument that a slider drag or a button click is not a deliberate act the way
 * a typed command is. That gate has been removed: the operator is now pointing
 * at a target on a map and watching the result, and the extra click was a step
 * between them and the command rather than a safety property. The section
 * records the reversal.
 *
 * The knobs below the map do not weaken this. A knob writes only the number in
 * the adjacent input; it sends nothing. The adjacent button remains the single
 * commit act, and it is the thing `data-motion` marks for the stale stand-down. */

function numArg(id: string): number {
    const el = req(id) as HTMLInputElement;
    const v = Number(el.value);
    if (!Number.isFinite(v)) {
        throw new Error(`#${id} is not a number`);
    }
    return v;
}

function numOrUndef(id: string): number | undefined {
    const raw = (req(id) as HTMLInputElement).value.trim();
    if (raw === '') {
        return undefined;
    }
    const v = Number(raw);
    if (!Number.isFinite(v)) {
        throw new Error(`#${id} is not a number`);
    }
    return v;
}

function clearParams(): void {
    for (const f of PARAM_FIELDS) {
        (req(f.id) as HTMLInputElement).value = '';
    }
    // Forgetting the arming-delay suggestion matters: it is the one field whose
    // value this console can arrive at on its own, and clearing the panel is the
    // operator saying "start from nothing".
    offsetTouched = false;
    renderOffsetSuggestion();
}

/* One SDR parameter: the input, the params.json key it maps to, the unit of the
 * value the operator types, and any legal-value hint.
 *
 * Single source of truth for four things that must not drift apart -- the input
 * id, the JSON key, the visible label and the unit -- because the rows below are
 * GENERATED from this table rather than written into the markup. They were
 * hand-written once and the units went missing when the markup was reorganised,
 * which is the drift this table exists to make impossible: there is now one place
 * to add a parameter and one place a key can be spelled wrong.
 *
 * Units are the unit of the value the operator TYPES, never a converted display
 * (GUI_ARCHITECTURE.md 9.4). For FS that is load-bearing rather than pedantry:
 * the contract bounds it at 1e5..61.44e6 Hz, so an operator who reads "MSPS" and
 * types 31.251 is refused by a bounds error about a number they believe they
 * entered correctly.
 *
 * `hint` is for fields whose legal values are enumerable. The antenna ports are
 * the only such fields, and the list is deliberately "known" rather than
 * exhaustive: UHD decides what this radio and firmware accept, and this table
 * has no business claiming to enumerate it. */
interface ParamField {
    id: string;
    key: string;
    label: string;
    unit: string;
    hint?: string;
    size: number;
}

/* Every key connect.cpp reads with j.at(), and nothing else. The sweep window,
 * the arming delay and the two antenna ports were absent for the life of this
 * console, which meant an operator could retune the radio but not change the
 * shape of the sweep it flies -- that took a hands-on edit to params.json on the
 * aircraft. PULSE_DURATION is deliberately still missing: config.hpp has its
 * read commented out, so a control for it would save a value the program never
 * looks at.
 *
 * Ordered as they appear in config.hpp's load_config, which is the order a reader
 * of that file meets them in. */
const PARAM_FIELDS: ParamField[] = [
    {id: 'in-prf', key: 'PRF', label: 'pulse rate', unit: 'Hz', size: 8},
    {id: 'in-fs', key: 'FS', label: 'sample rate', unit: 'Hz', size: 10},
    {id: 'in-sess', key: 'SESSION_DURATION', label: 'session', unit: 's', size: 6},
    {id: 'in-tx', key: 'TX_FREQ', label: 'tx freq', unit: 'Hz', size: 10},
    {id: 'in-gtx', key: 'NORMALIZED_GAIN_TX', label: 'gain tx', unit: '0–1', size: 6},
    {id: 'in-grx', key: 'NORMALIZED_GAIN_RX', label: 'gain rx', unit: '0–1', size: 6},
    {id: 'in-tmin', key: 'T_MIN_US', label: 'sweep from', unit: 'µs', size: 6},
    {id: 'in-tmax', key: 'T_MAX_US', label: 'sweep to', unit: 'µs', size: 6},
    {id: 'in-soffset', key: 'START_OFFSET_S', label: 'start offset', unit: 's', size: 6},
    {id: 'in-bw', key: 'BW', label: 'bandwidth', unit: 'Hz', size: 10},
    {
        id: 'in-txant', key: 'TX_ANTENNA', label: 'tx antenna', unit: '', size: 10,
        hint: 'known: TX/RX, RX2 — UHD decides',
    },
    {
        id: 'in-rxant', key: 'RX_ANTENNA', label: 'rx antenna', unit: '', size: 10,
        hint: 'known: TX/RX, RX2 — same port warns',
    },
];

/* paramRows builds the labelled rows for PARAM_FIELDS into host.
 *
 * One row per field, with the unit in the label, because several of these are
 * near neighbours numerically: FS and BW are both hertz, T_MIN_US and T_MAX_US
 * are both microseconds and must keep T_MIN < T_MAX, and a two-per-row layout
 * invites reading the wrong column. */
function paramRows(host: HTMLElement): void {
    for (const f of PARAM_FIELDS) {
        const row = document.createElement('div');
        row.className = 'row';

        const label = document.createElement('span');
        label.textContent = f.unit === '' ? f.label : `${f.label} (${f.unit})`;
        // The params.json key is in the title, because the operator may be
        // reading that file alongside the window.
        label.title = f.key;
        row.appendChild(label);

        const input = document.createElement('input');
        input.id = f.id;
        input.size = f.size;
        if (f.hint !== undefined) {
            input.title = f.hint;
        }
        row.appendChild(input);

        if (f.id === 'in-soffset') {
            // Filled by renderOffsetSuggestion. A span and not a button, because
            // it is only actionable while a suggestion is pending.
            const slot = document.createElement('span');
            slot.id = 'offset-suggestion';
            slot.className = 'hint';
            row.appendChild(slot);
        }
        host.appendChild(row);

        // Antenna hints also appear as text on the row, not only in a title
        // attribute: a hover hint is invisible to an operator in a hurry.
        if (f.hint !== undefined) {
            const note = document.createElement('div');
            note.className = 'row sub hint';
            note.textContent = `${f.key}: ${f.hint}`;
            host.appendChild(note);
        }
    }
}

/* Antenna ports are strings, so they are read differently from the numbers.
 * Blank means "leave alone" for both, and the distinction matters in the same
 * way it does for the numeric fields: an empty box is not a request to set an
 * antenna to nothing. */
function strOrUndef(id: string): string | undefined {
    const raw = (req(id) as HTMLInputElement).value.trim();
    return raw === '' ? undefined : raw;
}

/* Current values arrive as placeholder hints; inputs stay empty so blank keeps
 * meaning "leave alone". A read failure leaves "(unavailable)" hints rather
 * than stale numbers: a placeholder from a previous connection is a lie about
 * the current one. */
async function refreshParams(): Promise<void> {
    let vals: Record<string, unknown> | null = null;
    try {
        vals = JSON.parse(await call<string>('SdrGetParams')) as Record<string, unknown>;
    } catch (err) {
        logLine(`params read unavailable: ${err}`);
    }
    for (const f of PARAM_FIELDS) {
        const el = req(f.id) as HTMLInputElement;
        const v = vals === null ? undefined : vals[f.key];
        /* Numbers and strings are both real values here -- the antenna ports are
         * path names, not numbers -- so the hint accepts either. A key that is
         * present but of neither type means the Go side and this table have
         * genuinely diverged, and "(unknown)" is the honest way to say so rather
         * than printing a number the operator would take as a setting. */
        if (typeof v === 'number') {
            // A field with its own hint keeps it on screen and shows the file's
            // current value in the title, so the legal-value list stays where
            // the operator is about to type. Overwriting the hint with a number
            // would leave a control for an RF path showing "0.5".
            el.placeholder = f.hint === undefined ? String(v) : f.hint;
            el.title = f.hint === undefined ? f.key : `${f.key}: ${v}`;
        } else if (typeof v === 'string') {
            el.placeholder = f.hint === undefined ? v : `${v} (current)`;
            el.title = f.hint === undefined ? f.key : `${f.key}: ${v}`;
        } else if (f.hint !== undefined) {
            el.placeholder = f.hint;
            el.title = f.hint;
        } else {
            el.placeholder = vals === null ? '(unavailable)' : '(unknown)';
            el.title = f.key;
        }
    }
    if (vals !== null) {
        logLine('params refreshed from OBC');
    }
}

/* --------------------------------------------------------------------------
   Arming delay, suggested from the session length
   -------------------------------------------------------------------------- */

/* Arming delays that have been flown, keyed by session duration in seconds.
 *
 * The OBC does not derive this -- connect.cpp takes START_OFFSET_S as given --
 * so the relationship lives here, in the console, and is applied only when the
 * operator asks for it. Provenance is the whole content of this table: it is a
 * set of flown values, not a formula, and the nearest match is chosen because
 * interpolating a flight parameter would imply a confidence the data does not
 * have. */
const START_OFFSET_BY_DURATION: ReadonlyArray<readonly [number, number]> = [
    [1, 0.1],
    [5, 0.1],
    [10, 0.5],
    [15, 0.7],
    [20, 0.9],
    [25, 1.05],
];

/* offsetTouched records that the operator has typed their own arming delay.
 *
 * It gates the suggestion permanently, and that is the whole safety argument
 * for this feature. In this console blank means "leave alone", so ANY value in
 * the field becomes part of the next SetSdrParams whether or not the operator
 * meant it. Filling the field automatically -- which is what the Tkinter console
 * does, unconditionally, on every keystroke in SESSION_DURATION -- would mean
 * tabbing through the session field silently queued an arming-delay change for
 * the vehicle. So the suggestion is never written into the input; it is shown
 * beside it and takes one click.
 *
 * Once touched it stops appearing, because a suggestion that reappears over a
 * value somebody deliberately typed is noise at best and a warning at worst. */
let offsetTouched = false;

/* suggestStartOffset returns the flown arming delay nearest to durationS, or
 * undefined when the field is empty or not a number. */
function suggestStartOffset(durationS: number): number | undefined {
    let best: readonly [number, number] | undefined;
    for (const entry of START_OFFSET_BY_DURATION) {
        if (best === undefined || Math.abs(entry[0] - durationS) < Math.abs(best[0] - durationS)) {
            best = entry;
        }
    }
    return best?.[1];
}

/* renderOffsetSuggestion repaints the affordance beside the arming-delay input.
 *
 * Three states, and no fourth:
 *   - the operator typed a value: nothing. Their number stands unremarked.
 *   - a session duration is typed and the field is empty: a clickable chip
 *     carrying the flown value for that length.
 *   - no session duration: nothing. Guessing from an empty field is inventing a
 *     mission parameter.
 *
 * The chip states where the number came from rather than presenting it as the
 * value, because it is one: "flown at 20 s" is not "the value for 20 s". */
function renderOffsetSuggestion(): void {
    const slot = document.getElementById('offset-suggestion');
    if (slot === null) {
        return;
    }
    const offset = req('in-soffset') as HTMLInputElement;
    if (offsetTouched || offset.value.trim() !== '') {
        slot.textContent = '';
        return;
    }

    const raw = (req('in-sess') as HTMLInputElement).value.trim();
    if (raw === '') {
        slot.textContent = '';
        return;
    }
    const duration = Number(raw);
    if (!Number.isFinite(duration)) {
        slot.textContent = '';
        return;
    }
    const suggestion = suggestStartOffset(duration);
    if (suggestion === undefined) {
        slot.textContent = '';
        return;
    }

    slot.textContent = '';
    const chip = document.createElement('a');
    chip.href = '#';
    chip.className = 'suggest';
    chip.textContent = `use ${suggestion} s (flown at ${duration} s)`;
    chip.onclick = (ev: Event) => {
        ev.preventDefault();
        offset.value = String(suggestion);
        // Deliberate now: it came from a click, and further changes to the
        // session length must not overwrite it.
        offsetTouched = true;
        renderOffsetSuggestion();
    };
    slot.appendChild(chip);
}

/* Wire the two inputs the suggestion depends on.
 *
 * `input` rather than `change`, so the chip appears as the duration is typed
 * instead of only on blur. */
function wireOffsetSuggestion(): void {
    const offset = req('in-soffset') as HTMLInputElement;
    offset.addEventListener('input', () => {
        offsetTouched = true;
        renderOffsetSuggestion();
    });
    (req('in-sess') as HTMLInputElement).addEventListener('input', renderOffsetSuggestion);
}

/* Per-servo buttons are re-rendered with every frame, so they delegate from
 * the persistent container instead of being wired per render: wiring that has
 * to be re-attached every second is wiring that will be forgotten. */
function wireServoButtons(): void {
    req('pico').addEventListener('click', (ev: Event) => {
        const b = (ev.target as HTMLElement).closest('button');
        if (b === null) {
            return;
        }
        const zero = b.dataset.zeroServo;
        const stop = b.dataset.stopServo;
        if (zero !== undefined) {
            // Not "returns to its centre tick": `zero` teaches the servo the
            // tick it is already sitting at, so the axis does not move. That is
            // worth saying out loud, because the old wording implied motion and
            // an operator would reasonably brace for it.
            void run('zero', () => call<Cmd>('ZeroServo', Number(zero)));
        } else if (stop !== undefined) {
            void run('stop', () => call<Cmd>('StopServo', Number(stop)));
        }
    });
}

function strArg(id: string): string {
    return (req(id) as HTMLInputElement).value;
}

/* ------------------------------------------------------------------ */
/* frame and link                                                     */
/* ------------------------------------------------------------------ */

function renderFrame(ev: FrameEvent): void {
    const v = ev.view;
    currentView = v;
    // Only a real measurement goes into the history. Pushing an absent reading
    // as 0 would draw an idle link where the truth is "not measured".
    if (v.link.measured_tx_kbps !== null) {
        txHistory.push(v.link.measured_tx_kbps);
    }
    req('sys').innerHTML = renderSystem(v);
    req('gnss').innerHTML = renderGnss(v);
    req('pico').innerHTML = renderPico(v);
    req('csl').innerHTML = renderCameraSdrLink(v, txHistory.values());
    req('budget').innerHTML = renderBudgetTiles(v.link);
    req('attention').innerHTML = renderAttention(v, lastLink);
    const meta: string[] = [`seq ${v.sequence}`, `uptime ${Math.floor(v.uptime_s / 60)}m`];
    if (ev.gaps > 0) {
        meta.push(`gap +${ev.gaps}`);
    }
    if (ev.restart) {
        meta.push('OBC RESTARTED');
    }
    req('meta').textContent = meta.join(' · ');
    /* The frame event carries the link health as it was at this instant, so a
     * live link refreshes staleness on the 1 Hz frame as well as on the 2 s poll.
     * A replayed frame -- Snapshot() at first paint -- has no link, and must not
     * clear a stale verdict by looking recent. */
    if (ev.link !== undefined) {
        noteLink(ev.link);
    }
    refreshStale();
}

function renderLink(l: LinkState): void {
    lastLink = l;
    ageHistory.push(l.last_frame_age_s);
    req('linkline').innerHTML = renderLinkLine(l, ageHistory.values());
    noteLink(l);
    if (currentView !== null) {
        req('attention').innerHTML = renderAttention(currentView, l);
    }
}

/* Records the two numbers staleness is decided from. Deliberately separate from
 * renderLink: the link line is what the operator reads, and these are what the
 * motion gate reads, and conflating them is how one ends up gating on a
 * rendered string. */
function noteLink(l: LinkState): void {
    frameAgeS = l.last_frame_age_s;
    framesSeen = l.frames_received;
}

function refreshStale(): void {
    /* No frame ever is stale, not fresh. See the note on framesSeen: a console
     * launched against an unreachable OBC used to come up with every motion
     * button live, because age zero read as "a frame just arrived". */
    const stale = framesSeen === 0 || frameAgeS > STALE_AFTER_S;
    req('stale').style.display = stale ? 'block' : 'none';
    /* No frame for three intervals means the console looks at a picture of a
     * vehicle it can no longer reach. Motion stands down; everything else stays.
     * Panels dim but stay readable -- blanking them would hide exactly what the
     * operator needs while the link is down. */
    document.querySelectorAll('section').forEach((s) => {
        s.classList.toggle('stale-dim', stale);
    });
    document.querySelectorAll<HTMLElement>('[data-motion]').forEach((b) => {
        b.classList.toggle('disabled', stale);
        if (b instanceof HTMLButtonElement) {
            b.disabled = stale;
        }
    });
}

/* ------------------------------------------------------------------ */
/* artefacts                                                          */
/* ------------------------------------------------------------------ */

async function refreshListing(): Promise<void> {
    const box = req('files');
    try {
        const entries: Entry[] = await call<Entry[]>('ListArtefacts', currentPath);
        const up = currentPath !== ''
            ? `<div class="row"><a href="#" id="up">..</a></div>`
            : '';
        box.innerHTML = up + entries.map((e) => {
            if (e.directory) {
                return `<div class="row"><a href="#" data-dir="${esc(e.name)}">${esc(e.name)}/</a><span>dir</span></div>`;
            }
            /* Preview is offered for camera files only: a photograph renders in
             * the window, while a multi-megabyte SAR bin has no business near
             * an <img> tag. The Go side enforces the size cap regardless. */
            const preview = e.kind === 'camera'
                ? ` <button data-preview="${esc(e.name)}">preview</button>`
                : '';
            return `<div class="row"><span>${esc(e.name)}</span><span>${esc(e.kind)} ${fmtBytes(e.size_bytes)}</span> <button data-dl="${esc(e.name)}">fetch</button>${preview}</div>`;
        }).join('') || '<div class="row"><i>no artefacts</i></div>';
        req('pathline').textContent = `/${currentPath}`;
        box.querySelectorAll<HTMLAnchorElement>('[data-dir]').forEach((a) => {
            a.onclick = (ev: Event) => {
                ev.preventDefault();
                currentPath = currentPath === '' ? a.dataset.dir! : `${currentPath}/${a.dataset.dir!}`;
                void refreshListing();
            };
        });
        const upEl = document.getElementById('up');
        if (upEl !== null) {
            upEl.onclick = (ev: Event) => {
                ev.preventDefault();
                const parts = currentPath.split('/');
                parts.pop();
                currentPath = parts.join('/');
                void refreshListing();
            };
        }
        const files = entries.filter((e) => !e.directory);
        box.querySelectorAll<HTMLButtonElement>('[data-dl]').forEach((b) => {
            b.onclick = () => {
                void downloadByName(String(b.dataset.dl), files);
            };
        });
        box.querySelectorAll<HTMLButtonElement>('[data-preview]').forEach((b) => {
            b.onclick = () => {
                void showPreview(String(b.dataset.preview));
            };
        });
    } catch (err) {
        box.textContent = `listing failed: ${err}`;
    }
}

async function downloadByName(name: string, list: Entry[]): Promise<void> {
    if (!list.some((e) => e.name === name)) {
        return;
    }
    // No native file dialog: GTK's file chooser aborts the process when
    // GSettings schemas are missing, which is uncatchable. The destination is
    // typed instead, defaulting under ~/rocsar; the Go side expands the ~.
    const dir = strArg('in-destdir').trim() || '~/rocsar';
    const full = currentPath === '' ? name : `${currentPath}/${name}`;
    const dest = dir.endsWith('/') ? `${dir}${name}` : `${dir}/${name}`;
    try {
        await call<Cmd>('DownloadArtefact', full, dest);
        logLine(`download started: ${full} -> ${dest}`);
    } catch (err) {
        logLine(`download refused: ${err}`);
    }
}

/* The progress line is a bar with a wall-clock finish time, not a bare duration:
 * H1 makes progress and an ETA mandatory, and a 30 MB capture at 8 KiB/s is over
 * an hour, which is a time of day the operator wants, not a seconds count. */
function renderProgress(p: DownloadProgress): void {
    req('dlstatus').innerHTML = renderDownload(p.name, p.bytes, p.total, p.rate_kbs);
}

// showPreview fetches one artefact for in-window display. No auto-preview on
// capture: at 8 KiB/s every byte is budgeted, and a photograph the operator
// did not ask to see is bandwidth spent without consent.
async function showPreview(name: string): Promise<void> {
    const box = req('preview');
    box.innerHTML = `<i>loading ${esc(name)}…</i>`;
    try {
        const full = currentPath === '' ? name : `${currentPath}/${name}`;
        const url = await call<string>('PreviewArtefact', full);
        box.innerHTML = `<div class="row"><span>${esc(full)}</span><button id="preview-close">close</button></div>
            <img id="preview-img" alt="${esc(full)}">`;
        (req('preview-img') as HTMLImageElement).src = url;
        const close = document.getElementById('preview-close');
        if (close !== null) {
            close.onclick = () => {
                box.innerHTML = '<i>no preview</i>';
            };
        }
    } catch (err) {
        box.innerHTML = `<i>preview failed: ${esc(String(err))}</i>`;
        logLine(`preview ${name}: ${err}`);
    }
}

/* ------------------------------------------------------------------ */
/* knobs                                                              */
/* ------------------------------------------------------------------ */

/* A knob is a second view of the numeric input beside it, never a command.
 *
 * The input stays the source of truth, so numArg() and the commit buttons are
 * unchanged from what they were without knobs. The knob writes a value only
 * while the operator is dragging it; telemetry never writes into a command
 * input, which is why no "being edited" guard is needed and why nothing here
 * can fire a motion command. Dragging one cannot send a Jog.
 *
 * Units are the units of the command each knob sits beside -- degrees for
 * SetHeading and MountOffset, ticks for Jog -- so no conversion happens here
 * (GUI_ARCHITECTURE.md 9.4). A tick/degree conversion in the browser would be
 * exactly the kind of derived value that rule forbids. */
interface KnobParts {
    input: HTMLInputElement;
    min: number;
    max: number;
    step: number;
    size: number;
}

function knobParts(svg: SVGElement): KnobParts | null {
    const forId = svg.dataset.knobFor;
    if (forId === undefined) {
        return null;
    }
    const input = document.getElementById(forId);
    if (!(input instanceof HTMLInputElement)) {
        return null;
    }
    return {
        input,
        min: Number(svg.dataset.min),
        max: Number(svg.dataset.max),
        step: Number(svg.dataset.step) || 1,
        size: Number(svg.dataset.size) || 64,
    };
}

function paintKnob(svg: SVGElement, p: KnobParts, value: number): void {
    const g = knobGeometry(value, p.min, p.max, p.size);
    const line = svg.querySelector('.w-knob-needle');
    if (line !== null) {
        line.setAttribute('x1', g.x1.toFixed(2));
        line.setAttribute('y1', g.y1.toFixed(2));
        line.setAttribute('x2', g.x2.toFixed(2));
        line.setAttribute('y2', g.y2.toFixed(2));
    }
    svg.setAttribute('aria-valuenow', String(value));
}

function wireKnobs(): void {
    document.querySelectorAll<SVGElement>('svg.w-knob').forEach((svg) => {
        const p = knobParts(svg);
        if (p === null) {
            return;
        }
        let dragging = false;

        const setFromPointer = (ev: PointerEvent): void => {
            const r = svg.getBoundingClientRect();
            const dx = ev.clientX - (r.left + r.width / 2);
            const dy = ev.clientY - (r.top + r.height / 2);
            const v = knobValueFromPointer(p.min, p.max, p.step, dx, dy);
            p.input.value = String(v);
            paintKnob(svg, p, v);
        };

        svg.addEventListener('pointerdown', (ev: PointerEvent) => {
            dragging = true;
            svg.focus();
            svg.setPointerCapture(ev.pointerId);
            setFromPointer(ev);
            ev.preventDefault();
        });
        svg.addEventListener('pointermove', (ev: PointerEvent) => {
            if (dragging) {
                setFromPointer(ev);
            }
        });
        const release = (ev: PointerEvent): void => {
            dragging = false;
            if (svg.hasPointerCapture(ev.pointerId)) {
                svg.releasePointerCapture(ev.pointerId);
            }
        };
        svg.addEventListener('pointerup', release);
        svg.addEventListener('pointercancel', release);

        // Keyboard, because a slider that can only be dragged is unusable
        // without a mouse. Same keys a range input answers to.
        svg.addEventListener('keydown', (ev: KeyboardEvent) => {
            const v = Number(p.input.value) || 0;
            let next = v;
            if (ev.key === 'ArrowRight' || ev.key === 'ArrowUp') {
                next = v + p.step;
            } else if (ev.key === 'ArrowLeft' || ev.key === 'ArrowDown') {
                next = v - p.step;
            } else if (ev.key === 'Home') {
                next = p.min;
            } else if (ev.key === 'End') {
                next = p.max;
            } else {
                return;
            }
            ev.preventDefault();
            next = Math.min(p.max, Math.max(p.min, Number(next.toFixed(6))));
            p.input.value = String(next);
            paintKnob(svg, p, next);
        });

        // Typing a value moves the dial; the knob is a view of the input.
        p.input.addEventListener('input', () => paintKnob(svg, p, Number(p.input.value) || 0));
    });
}

/* ------------------------------------------------------------------ */
/* wiring                                                             */
/* ------------------------------------------------------------------ */

function wireCommands(): void {
    const on = (id: string, fn: () => void): void => {
        req(id).addEventListener('click', () => {
            void (async () => {
                try {
                    fn();
                } catch (err) {
                    logLine(`input: ${err}`);
                }
            })();
        });
    };
    on('b-query', () => void call<Cmd>('QueryStatus').then((r) => cmdLine('query', r)));
    on('b-photo', () => void call<Cmd>('TakePhoto').then((r) => {
        cmdLine('photo', r);
        if (r.success) {
            void refreshListing();
        }
    }));
    on('b-gnss', () => void call<Cmd>('SelectReceiver', numArg('in-gnss')).then((r) => cmdLine('gnss', r)));
    on('b-gnss-rot', () => void call<Cmd>('RotateReceiver').then((r) => cmdLine('gnss-rotate', r)));
    on('b-heading', () => void run('heading', () => call<Cmd>('SetHeading', numArg('in-heading'))));
    on('b-jog', () => void run('jog', () => call<Cmd>('Jog', numArg('in-jog-id'), numArg('in-jog-tick'))));
    on('b-zero', () => void run('zero', () => call<Cmd>('ZeroAll')));
    on('b-mount', () => void run('mount', () => call<Cmd>('MountOffset', numArg('in-mount-id'), numArg('in-mount-deg'))));
    on('b-dir', () => void run('dir', () => call<Cmd>('SetDirection', numArg('in-dir-id'), numArg('in-dir-m'))));
    on('b-heater', () => void run('heater', () => call<Cmd>('SetHeater', numArg('in-heater-id'), strArg('in-heater-state') === 'on')));
    on('b-stop', () => void run('stop', () => call<Cmd>('StopAll')));
    on('b-pico', () => void call<Cmd>('PicoStatusRequest').then((r) => cmdLine('pico-status', r)));
    on('b-probe', () => void call<Cmd>('SdrProbe').then((r) => cmdLine('sdr-probe', r)));
    on('b-sdrcon', () => void call<Cmd>('SdrConnect').then((r) => cmdLine('sdr-connect', r)));
    on('b-sdrusb', () => void call<Cmd>('SdrResetUSB').then((r) => cmdLine('sdr-reset-usb', r)));
    on('b-link', () => void call<Cmd>('SetLinkLimit', numArg('in-link')).then((r) => cmdLine('link', r)));
    on('b-telrate', () => void call<Cmd>('SetTelemetryRate', numArg('in-telrate')).then((r) => cmdLine('telrate', r)));
    on('b-params', () => void (async () => {
        try {
            const r = await call<Cmd>('SetSdrParams', {
                prf_hz: numOrUndef('in-prf'),
                sample_rate_hz: numOrUndef('in-fs'),
                tx_freq_hz: numOrUndef('in-tx'),
                normalized_gain_tx: numOrUndef('in-gtx'),
                normalized_gain_rx: numOrUndef('in-grx'),
                bandwidth_hz: numOrUndef('in-bw'),
                session_duration_s: numOrUndef('in-sess'),
                t_min_us: numOrUndef('in-tmin'),
                t_max_us: numOrUndef('in-tmax'),
                start_offset_s: numOrUndef('in-soffset'),
                tx_antenna: strOrUndef('in-txant'),
                rx_antenna: strOrUndef('in-rxant'),
            } as SdrParamsPatch);
            cmdLine('sdr-set-params', r);
            if (r.success) {
                clearParams();
                await refreshParams();
            }
        } catch (err) {
            logLine(`params input: ${err}`);
        }
    })());
    on('b-params-clear', () => clearParams());
    on('b-params-refresh', () => void refreshParams());
    on('b-ls', () => void refreshListing());
    on('b-dlcancel', () => void call<Cmd>('CancelDownload').then(
        () => logLine('download cancel requested'),
        (err: unknown) => logLine(`cancel: ${err}`),
    ));
    on('b-conn', () => void (async () => {
        try {
            await call<Cmd>('Connect', {
                control_endpoint: strArg('in-control'),
                telemetry_endpoint: strArg('in-telemetry'),
                http_endpoint: strArg('in-http'),
                topic: 'telemetry',
            } as Config);
            logLine('connect requested');
        } catch (err) {
            logLine(`connect: ${err}`);
        }
    })());
    on('b-disconn', () => void call<Cmd>('Disconnect').then(() => logLine('disconnected')));
}

function wireEvents(): void {
    on('telemetry:frame', (ev) => {
        renderFrame(ev as FrameEvent);
    });
    on('link:state', (ev) => renderLink(ev as LinkState));
    on('command:result', (ev) => {
        const r = ev as Cmd;
        logLine(`result: success=${r.success} error=${r.error} ${r.message}`);
    });
    on('download:progress', (ev) => {
        renderProgress(ev as DownloadProgress);
    });
    on('download:done', (ev) => {
        const d = ev as DownloadDone;
        logLine(d.error !== '' ? `download FAILED ${d.name}: ${d.error}` : `download done ${d.name}: ${fmtBytes(d.bytes)}`);
        req('dlstatus').textContent = d.error !== '' ? `failed: ${d.error}` : `done: ${d.name}`;
    });
    on('log', (msg) => logLine(String(msg)));
}

/* ------------------------------------------------------------------ */
/* layout                                                             */
/* ------------------------------------------------------------------ */

/* Map-dominant, in three zones.
 *
 * The map is what the operator watches and points at, so it takes the large
 * left cell and the compass sits in the flight-controller panel beside it. The
 * right rail carries the controls and the per-axis cards. The bottom strip --
 * artefacts, preview, log -- is reference material rather than a live reading,
 * so it is collapsible with native <details>. Below ~1100 px the whole thing
 * falls to one column, same as it always did. */
function layout(): void {
    req('app').innerHTML = `
    <header>
      <h1>ROCSAR Ground Station</h1>
      <div class="conn">
        <input id="in-control" size="22" title="control endpoint">
        <input id="in-telemetry" size="22" title="telemetry endpoint">
        <input id="in-http" size="20" title="artefact server">
        <button id="b-conn">connect</button>
        <button id="b-disconn">disconnect</button>
      </div>
    </header>
    <div id="statusbar">
      <div id="linkline" class="status-link">…</div>
      <div id="budget" class="status-budget"></div>
      <div id="meta" class="status-meta"></div>
    </div>
    <div id="stale" class="banner err" style="display:none">LINK STALE — showing last known values; motion stands down</div>
    <div id="attention" class="attention"></div>
    <div id="layout">
      <div class="zone-main">
        <section class="p-pico motion-zone"><h2>flight controller</h2><div id="pico"></div>
          <div class="controls">
            <div class="ctl">
              <span class="ctl-label">heading (deg)</span>
              ${knob({ forId: 'in-heading', label: 'target heading', min: 0, max: 359, step: 1, value: 0, unit: '°' })}
              <input id="in-heading" size="5" value="0" title="target heading in degrees">
              <button data-motion id="b-heading">set heading</button>
              <button id="b-pico">status</button>
            </div>
            <div class="ctl">
              <span class="ctl-label">jog (tick 0–4095)</span>
              ${knob({ forId: 'in-jog-tick', label: 'jog tick position', min: 0, max: 4095, step: 1, value: 2048, unit: 'tick' })}
              <input id="in-jog-id" size="2" value="1" title="servo id">
              <input id="in-jog-tick" size="5" value="2048" title="absolute tick, 0–4095">
              <button data-motion id="b-jog">jog</button>
              <button data-motion id="b-zero">zero both</button>
            </div>
            <div class="ctl">
              <span class="ctl-label">mount offset (deg)</span>
              ${knob({ forId: 'in-mount-deg', label: 'mount offset', min: -180, max: 180, step: 1, value: 0, unit: '°' })}
              <input id="in-mount-id" size="2" value="1" title="servo id">
              <input id="in-mount-deg" size="5" value="0" title="mount offset in degrees">
              <button data-motion id="b-mount">mount</button>
            </div>
            <div class="ctl">
              <span class="ctl-label">direction</span>
              <input id="in-dir-id" size="2" value="1" title="servo id">
              <input id="in-dir-m" size="3" value="+1" title="+1 or -1">
              <button data-motion id="b-dir">direction</button>
            </div>
            <div class="ctl">
              <span class="ctl-label">heater</span>
              <input id="in-heater-id" size="2" value="1" title="heater id">
              <input id="in-heater-state" size="4" value="on" title="on or off">
              <button data-motion id="b-heater">heater</button>
            </div>
            <div class="ctl ctl-stop">
              <button data-motion id="b-stop" class="stop">STOP ALL</button>
            </div>
          </div>
        </section>
        <section class="p-map"><h2>map <span class="hint">click to point both antennas</span></h2>
          <div id="map"></div>
          <div id="position"></div>
        </section>
      </div>
      <div class="zone-rail">
        <section class="p-sys"><h2>system</h2><div id="sys"></div></section>
        <section class="p-gnss"><h2>gnss</h2><div id="gnss"></div>
          <div class="row"><input id="in-gnss" size="4" value="1"><button id="b-gnss">select</button> <button id="b-gnss-rot">rotate</button></div>
        </section>
        <section class="p-csl"><h2>camera · sdr · link</h2><div id="csl"></div>
          <div class="row"><button id="b-photo">photo</button> <button id="b-probe">sdr probe</button> <button id="b-sdrcon">sdr connect</button> <button id="b-sdrusb">sdr reset usb</button></div>
          <div class="row"><input id="in-link" size="6" value="115"><button id="b-link">set limit kbit</button> <input id="in-telrate" size="5" value="1000"><button id="b-telrate">set telemetry ms</button> <button id="b-query">query</button></div>
          <div class="row" id="cmd-result"></div>
          <div class="row"><span>sdr params (blank = leave alone)</span></div>
          <div id="param-rows"></div>
          <div class="row"><button id="b-params">apply</button> <button id="b-params-clear">clear</button> <button id="b-params-refresh">refresh</button></div>
        </section>
      </div>
      <div class="zone-bottom">
        <details class="panel-fold" open><summary>artefacts <span id="pathline">/</span></summary>
          <section class="p-art"><div id="files"></div>
            <div class="row"><button id="b-ls">refresh</button> <button id="b-dlcancel">cancel download</button></div>
            <div class="row"><span>save to</span><input id="in-destdir" size="24" value="~/rocsar"></div>
            <div class="row" id="dlstatus"></div>
          </section>
        </details>
        <details class="panel-fold" open><summary>preview</summary>
          <section class="p-prev"><div id="preview"><i>no preview — fetch one from a camera file above</i></div></section>
        </details>
        <details class="panel-fold" open><summary>log</summary>
          <section class="p-log"><div id="log"></div></section>
        </details>
      </div>
    </div>`;

    // After the markup exists and before anything can call req() on a parameter
    // input: the rows are generated from PARAM_FIELDS, so the inputs do not
    // exist until this line runs.
    paramRows(req('param-rows'));
}

async function firstPaint(): Promise<void> {
    try {
        const [eps, snap, link] = await Promise.all([
            call<Config>('Endpoints'),
            call<View | null>('Snapshot'),
            call<LinkState>('LinkState'),
        ]);
        (req('in-control') as HTMLInputElement).value = eps.control_endpoint;
        (req('in-telemetry') as HTMLInputElement).value = eps.telemetry_endpoint;
        (req('in-http') as HTMLInputElement).value = eps.http_endpoint;
        renderLink(link);
        if (snap !== null) {
            renderFrame({ view: snap, gaps: 0, restart: false });
        }
        await refreshListing();
        await refreshParams();
    } catch (err) {
        logLine(`first paint: ${err}`);
    }
}

async function boot(): Promise<void> {
    await connect();
    layout();
    initMap();
    /* A click gives a position; a heading command takes a bearing. They are not
     * the same number, and sending the latitude was a real bug: bearingTo
     * computes the great-circle initial bearing from the gondola to the clicked
     * point, and refuses when there is no fix to measure from. */
    setMapClickHandler((lat, lon) => {
        const bearing = bearingTo(lat, lon);
        if (bearing === null) {
            logLine('map click ignored: no position fix yet');
            return;
        }
        void run('map-click', () => call<Cmd>('SetHeading', bearing));
    });
    // Viewport and version go to the log first: layout complaints ("single
    // column") are undecidable without the CSS pixel width, and binary
    // provenance questions end at the stamped version, not in a thread.
    logLine(`viewport ${window.innerWidth}x${window.innerHeight}`);
    void call<string>('Version').then(
        (v) => logLine(`gs version ${v}`),
        (err: unknown) => logLine(`version: ${err}`),
    );
    wireEvents();
    wireCommands();
    wireServoButtons();
    wireKnobs();
    wireOffsetSuggestion();
    setInterval(refreshStale, 1000);
    setInterval(() => {
        void call<LinkState>('LinkState').then(renderLink, (err: unknown) => logLine(`link poll: ${err}`));
    }, 2000);
    await firstPaint();
}

void boot();
