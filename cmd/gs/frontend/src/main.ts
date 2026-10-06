/* ROCSAR Ground Station.
 *
 * Vanilla TypeScript, no framework. The window renders what Go decodes: every
 * value arrives through the WebSocket bridge (first paint) and events
 * (everything after), already shaped by internal/gsview. This file formats
 * and places; it never converts units, never invents a zero, and never
 * constructs a command -- motion commands go through confirm() per
 * GUI_ARCHITECTURE.md 10.
 *
 * The console is a pure web app: the Go binary serves this file and the
 * operator opens the URL in their own browser. There is no generated binding
 * layer -- bridge.ts is the hand-written client and models.ts the hand-written
 * types. See GUI_ARCHITECTURE.md sections 1.2-1.4.
 */
import 'leaflet/dist/leaflet.css';
import { initMap } from './map';
import './style.css';
import { call, on, connect } from './bridge';
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

function req(id: string): HTMLElement {
    const el = document.getElementById(id);
    if (el === null) {
        throw new Error(`missing element #${id}`);
    }
    return el;
}

function esc(s: string): string {
    return s.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;');
}

function fmtDeg(v: number): string {
    return `${v.toFixed(2)}°`;
}

function fmtUptime(s: number): string {
    const h = Math.floor(s / 3600);
    const m = Math.floor((s % 3600) / 60);
    const sec = Math.floor(s % 60);
    return `${h}h${m.toString().padStart(2, '0')}m${sec.toString().padStart(2, '0')}s`;
}

function fmtBytes(n: number): string {
    if (n < 1024) {
        return `${n} B`;
    }
    if (n < 1024 * 1024) {
        return `${(n / 1024).toFixed(1)} KiB`;
    }
    return `${(n / 1024 / 1024).toFixed(1)} MiB`;
}

function fmtAge(s: number): string {
    if (s < 1.5) {
        return 'just now';
    }
    if (s < 90) {
        return `${s.toFixed(1)}s ago`;
    }
    return `${(s / 60).toFixed(1)}m ago`;
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

/* Motion commands ask first. A typed `jog 1 3000` in a terminal is a
 * deliberate act with an echo; a click is neither. See GUI_ARCHITECTURE.md 10.
 * One overlay per call, removed on settle: concurrent prompts stack rather
 * than replacing each other, so no confirmation can be dismissed by another
 * button's dialog. */
function confirmMotion(what: string, detail: string): Promise<boolean> {
    return new Promise((resolve) => {
        const overlay = document.createElement('div');
        overlay.className = 'modal-overlay';
        overlay.innerHTML = `
            <div class="modal" role="alertdialog" aria-modal="true" aria-label="confirm motion">
                <div class="modal-title">Confirm motion</div>
                <div class="modal-what"></div>
                <div class="modal-detail"></div>
                <div class="modal-buttons">
                    <button class="modal-cancel">Cancel (Esc)</button>
                    <button class="modal-confirm">Confirm</button>
                </div>
            </div>`;
        (overlay.querySelector('.modal-what') as HTMLElement).textContent = what;
        (overlay.querySelector('.modal-detail') as HTMLElement).textContent = detail;
        const done = (v: boolean): void => {
            document.removeEventListener('keydown', onKey);
            overlay.remove();
            resolve(v);
        };
        const onKey = (ev: KeyboardEvent): void => {
            if (ev.key === 'Escape') {
                done(false);
            } else if (ev.key === 'Enter') {
                done(true);
            }
        };
        (overlay.querySelector('.modal-cancel') as HTMLButtonElement).onclick = () => done(false);
        (overlay.querySelector('.modal-confirm') as HTMLButtonElement).onclick = () => done(true);
        overlay.addEventListener('mousedown', (ev: Event) => {
            if (ev.target === overlay) {
                done(false);
            }
        });
        document.addEventListener('keydown', onKey);
        document.body.appendChild(overlay);
        (overlay.querySelector('.modal-confirm') as HTMLButtonElement).focus();
    });
}

async function runMotion(name: string, what: string, detail: string, fn: () => Promise<Cmd>): Promise<void> {
    if (!(await confirmMotion(what, detail))) {
        return;
    }
    await run(name, fn);
}

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
    for (const id of ['in-prf', 'in-fs', 'in-tx', 'in-gtx', 'in-grx', 'in-bw', 'in-sess']) {
        (req(id) as HTMLInputElement).value = '';
    }
}

/* Input id to params.json key. One table, used for both directions: refresh
 * writes placeholders from it, apply reads values for it. A key renamed on
 * either side breaks visibly (placeholder shows "(unknown)") rather than
 * silently mapping to the wrong field. */
const PARAM_FIELDS: Array<[string, string]> = [
    ['in-prf', 'PRF'],
    ['in-fs', 'FS'],
    ['in-tx', 'TX_FREQ'],
    ['in-gtx', 'NORMALIZED_GAIN_TX'],
    ['in-grx', 'NORMALIZED_GAIN_RX'],
    ['in-bw', 'BW'],
    ['in-sess', 'SESSION_DURATION'],
];

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
    for (const [id, key] of PARAM_FIELDS) {
        const el = req(id) as HTMLInputElement;
        const v = vals === null ? undefined : vals[key];
        el.placeholder = typeof v === 'number' ? String(v) : '(unknown)';
    }
    if (vals !== null) {
        logLine('params refreshed from OBC');
    }
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
            void runMotion('zero', `Centre servo ${zero}?`, 'The axis returns to its centre tick.', () => call<Cmd>('ZeroServo', Number(zero)));
        } else if (stop !== undefined) {
            void runMotion('stop', `Stop servo ${stop}?`, 'Motion halts on that axis.', () => call<Cmd>('StopServo', Number(stop)));
        }
    });
}

function strArg(id: string): string {
    return (req(id) as HTMLInputElement).value;
}

/* ------------------------------------------------------------------ */
/* panels                                                             */
/* ------------------------------------------------------------------ */

function renderSystem(v: View): string {
    const s = v.system;
    const mocked = s.mocked && s.mocked.length > 0
        ? `<div class="banner warn">SIMULATED: ${s.mocked.map(esc).join(', ')} — fabricated, not measured</div>`
        : '';
    return `${mocked}
        <div class="row"><span>state</span><b>${esc(s.state)}</b></div>
        <div class="row"><span>uptime</span><b>${fmtUptime(s.uptime_s)}</b></div>
        <div class="row"><span>cpu</span><b>${s.cpu_temp_c.toFixed(0)} °C</b></div>`;
}

function renderGnss(v: View): string {
    return v.gnss.map((g) => {
        const mark = g.selected ? '*' : ' ';
        const pos = g.position === null || g.position === undefined
            ? '<i>no fix — not a position</i>'
            : `${g.position.latitude_deg.toFixed(6)}, ${g.position.longitude_deg.toFixed(6)} alt ${g.position.altitude_m.toFixed(1)} m`;
        const age = g.fix_age_s === null || g.fix_age_s === undefined ? 'never' : fmtAge(g.fix_age_s);
        return `<div class="row"><span>gnss${mark} ${g.receiver_id}</span><b>${pos}</b></div>
            <div class="row sub"><span>fix</span><b>${g.fix_ok ? `ok, ${age}` : 'no fix'}</b></div>
            <div class="row sub"><span>packets</span><b>ok ${g.packets_accepted} / rej ${g.packets_rejected}</b></div>`;
    }).join('');
}

function renderPico(v: View): string {
    if (v.pico === null || v.pico === undefined) {
        return '<div class="row"><span>pico</span><b>no data</b></div>';
    }
    const p = v.pico;
    const axes = (p.antennas || []).map((a) => {
        const load = a.load === null || a.load === undefined
            ? 'load held'
            : `load ${a.load}% ${a.temperature_c ?? '?'}C`;
        const fault = a.feedback_error ? ` <span class="err">FAULT ${esc(a.feedback_error)}</span>` : '';
        return `<div class="row sub"><span>servo ${a.servo_id}</span><b>tick ${a.current_tick} ${fmtDeg(a.current_angle_deg)} ${esc(load)} ${esc(a.feedback)}${fault}</b> <button data-motion data-zero-servo="${a.servo_id}">zero</button> <button data-motion data-stop-servo="${a.servo_id}">stop</button></div>`;
    }).join('');
    const ack = v.pico_last_ack === null || v.pico_last_ack === undefined
        ? 'no ack'
        : `ack #${v.pico_last_ack.command_sequence} ${v.pico_last_ack.success ? 'ok' : esc(v.pico_last_ack.error)}`;
    const imuTemp = p.imu_temperature_c === null || p.imu_temperature_c === undefined
        ? '—'
        : `${p.imu_temperature_c.toFixed(1)}C`;
    return `<div class="row"><span>pico</span><b>${v.pico_connected ? 'connected' : 'NOT connected'} heading ${fmtDeg(p.gondola_heading_deg)} (target ${fmtDeg(p.target_heading_deg)}) imu ${esc(p.imu)}</b></div>
        <div class="row sub"><span>heaters</span><b>${p.heater1_on ? 'on' : 'off'} / ${p.heater2_on ? 'on' : 'off'} · ${esc(ack)}</b></div>
        <div class="row sub"><span>imu temp</span><b>${imuTemp}</b></div>
        ${axes}`;
}

function renderCameraSdrLink(v: View): string {
    const c = v.camera;
    const photos = c.photos_taken === null || c.photos_taken === undefined ? '—' : `${c.photos_taken} photo(s)`;
    const last = c.last_photo ? ` last ${esc(c.last_photo)}` : '';
    const s = v.sdr;
    const log = s.last_error ? ` last error: ${esc(s.last_error)}` : '';
    const l = v.link;
    const shaping = l.shaping_active ? 'shaping on' : `shaping off${l.inactive_reason ? `: ${esc(l.inactive_reason)}` : ''}`;
    return `<div class="row"><span>camera</span><b>${esc(c.state)} ${photos}${last}</b></div>
        <div class="row"><span>sdr</span><b>${esc(s.state)}${s.running ? ` running pid ${s.running}` : ''}${log}</b></div>
        <div class="row"><span>link</span><b>${esc(l.state)} ${esc(l.device)} ${l.rate_kbps} kbit/s ${shaping}</b></div>`;
}

function renderHealth(v: View): string {
    const bad = !v.healthy || (v.not_healthy_reasons && v.not_healthy_reasons.length > 0);
    if (!bad) {
        return '';
    }
    const reasons = (v.not_healthy_reasons || []).map(esc).join('; ');
    return `<div class="banner err">not healthy${reasons ? `: ${reasons}` : ''}</div>`;
}

function renderFrame(ev: FrameEvent): void {
    const v = ev.view;
    req('sys').innerHTML = renderSystem(v);
    req('gnss').innerHTML = renderGnss(v);
    req('pico').innerHTML = renderPico(v);
    req('csl').innerHTML = renderCameraSdrLink(v);
    req('health').innerHTML = renderHealth(v);
    const meta: string[] = [`seq ${v.sequence}`];
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
    const dot = (ok: boolean): string => ok ? '●' : '○';
    req('linkline').innerHTML =
        `<span class="${l.control_connected ? 'ok' : 'bad'}">${dot(l.control_connected)} control</span> ` +
        `<span class="${l.telemetry_connected ? 'ok' : 'bad'}">${dot(l.telemetry_connected)} telemetry</span> ` +
        `<span>frames ${l.frames_received} gaps ${l.sequence_gaps} dropped ${l.frames_discarded}</span> ` +
        `<span>${l.last_error ? esc(l.last_error) : ''}</span>`;
    noteLink(l);
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

function renderProgress(p: DownloadProgress): void {
    const pct = p.total > 0 ? ` ${(100 * p.bytes / p.total).toFixed(1)}%` : '';
    const eta = p.rate_kbs > 0.1 && p.total > 0
        ? ` ETA ${Math.round((p.total - p.bytes) / 1024 / p.rate_kbs)}s`
        : '';
    req('dlstatus').textContent =
        `${p.name}: ${fmtBytes(p.bytes)}${p.total > 0 ? ` / ${fmtBytes(p.total)}` : ''}${pct} ${p.rate_kbs.toFixed(1)} KiB/s${eta}`;
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
    on('b-heading', () => void runMotion('heading', `Point both axes at ${strArg('in-heading')}°?`, 'The target bearing changes on acknowledge. Check the number — there is no undo.', () => call<Cmd>('SetHeading', numArg('in-heading'))));
    on('b-jog', () => void runMotion('jog', `Jog servo ${strArg('in-jog-id')} to tick ${strArg('in-jog-tick')}?`, 'Absolute tick move in manual mode. Out-of-range ticks are refused before sending.', () => call<Cmd>('Jog', numArg('in-jog-id'), numArg('in-jog-tick'))));
    on('b-zero', () => void runMotion('zero', 'Centre both axes?', 'Each axis returns to its centre tick.', () => call<Cmd>('ZeroAll')));
    on('b-mount', () => void runMotion('mount', `Set mount offset of servo ${strArg('in-mount-id')} to ${strArg('in-mount-deg')}°?`, 'Stored on the flight controller; affects subsequent pointing.', () => call<Cmd>('MountOffset', numArg('in-mount-id'), numArg('in-mount-deg'))));
    on('b-dir', () => void runMotion('dir', `Set direction of servo ${strArg('in-dir-id')} to ${strArg('in-dir-m')}?`, 'Reverses the axis sense. Check the sign — there is no undo.', () => call<Cmd>('SetDirection', numArg('in-dir-id'), numArg('in-dir-m'))));
    on('b-heater', () => void runMotion('heater', `Switch heater ${strArg('in-heater-id')} ${strArg('in-heater-state')}?`, 'Thermal control; takes effect on acknowledge.', () => call<Cmd>('SetHeater', numArg('in-heater-id'), strArg('in-heater-state') === 'on')));
    on('b-stop', () => void runMotion('stop', 'Stop every axis?', 'Motion halts on every axis.', () => call<Cmd>('StopAll')));
    on('b-pico', () => void call<Cmd>('PicoStatusRequest').then((r) => cmdLine('pico-status', r)));
    on('b-probe', () => void call<Cmd>('SdrProbe').then((r) => cmdLine('sdr-probe', r)));
    on('b-sdrcon', () => void call<Cmd>('SdrConnect').then((r) => cmdLine('sdr-connect', r)));
    on('b-sdrusb', () => void call<Cmd>('SdrResetUSB').then((r) => cmdLine('sdr-reset-usb', r)));
    on('b-link', () => void call<Cmd>('SetLinkLimit', numArg('in-link')).then((r) => cmdLine('link', r)));
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

function layout(): void {
    req('app').innerHTML = `
    <header>
      <h1>ROCSAR Ground Station</h1>
      <div id="linkline">…</div>
      <div class="conn">
        <input id="in-control" size="22" title="control endpoint">
        <input id="in-telemetry" size="22" title="telemetry endpoint">
        <input id="in-http" size="20" title="artefact server">
        <button id="b-conn">connect</button>
        <button id="b-disconn">disconnect</button>
      </div>
    </header>
    <div id="stale" class="banner err" style="display:none">LINK STALE — showing last known values; motion stands down</div>
    <div id="health"></div>
    <div id="meta"></div>
    <div id="panels">
    <section class="p-sys"><h2>system</h2><div id="sys"></div></section>
    <section class="p-gnss"><h2>gnss</h2><div id="gnss"></div>
      <div class="row"><input id="in-gnss" size="4" value="1"><button id="b-gnss">select</button> <button id="b-gnss-rot">rotate</button></div>
    </section>
    <section class="p-pico motion-zone"><h2>flight controller</h2><div id="pico"></div>
      <div class="row"><input id="in-heading" size="8" value="0"><button data-motion id="b-heading">set heading</button> <button id="b-pico">status</button></div>
      <div class="row"><input id="in-jog-id" size="3" value="1"><input id="in-jog-tick" size="6" value="2048"><button data-motion id="b-jog">jog</button> <button data-motion id="b-zero">zero both</button></div>
      <div class="row"><input id="in-mount-id" size="3" value="1"><input id="in-mount-deg" size="6" value="0"><button data-motion id="b-mount">mount</button></div>
      <div class="row"><input id="in-dir-id" size="3" value="1"><input id="in-dir-m" size="4" value="+1"><button data-motion id="b-dir">direction</button></div>
      <div class="row"><input id="in-heater-id" size="3" value="1"><input id="in-heater-state" size="4" value="on"><button data-motion id="b-heater">heater</button></div>
      <div class="row"><button data-motion id="b-stop">stop all</button></div>
    </section>
    <section class="p-map"><h2>map</h2>
      <div id="map" style="height: 400px; width: 100%; margin-top: 8px;"></div>
      <div id="position" style="font-size: 12px; color: var(--dim); margin-top: 4px;"></div>
    </section>
    <section class="p-csl"><h2>camera · sdr · link</h2><div id="csl"></div>
      <div class="row"><button id="b-photo">photo</button> <button id="b-probe">sdr probe</button> <button id="b-sdrcon">sdr connect</button> <button id="b-sdrusb">sdr reset usb</button></div>
      <div class="row"><input id="in-link" size="6" value="115"><button id="b-link">set limit kbit</button> <button id="b-query">query</button></div>
      <div class="row" id="cmd-result"></div>
      <div class="row"><span>sdr params (blank = leave alone)</span></div>
      <div class="row"><span>prf</span><input id="in-prf" size="8"><span>fs</span><input id="in-fs" size="10"><span>tx</span><input id="in-tx" size="10"></div>
      <div class="row"><span>gain tx</span><input id="in-gtx" size="6"><span>gain rx</span><input id="in-grx" size="6"><span>bw</span><input id="in-bw" size="10"></div>
      <div class="row"><span>session s</span><input id="in-sess" size="6"><button id="b-params">apply</button> <button id="b-params-clear">clear</button> <button id="b-params-refresh">refresh</button></div>
    </section>
    <section class="p-art"><h2>artefacts <span id="pathline">/</span></h2><div id="files"></div>
      <div class="row"><button id="b-ls">refresh</button> <button id="b-dlcancel">cancel download</button></div>
      <div class="row"><span>save to</span><input id="in-destdir" size="24" value="~/rocsar"></div>
      <div class="row" id="dlstatus"></div>
    </section>
    <section class="p-prev"><h2>preview</h2><div id="preview"><i>no preview — fetch one from a camera file above</i></div></section>
    <section class="p-log"><h2>log</h2><div id="log"></div></section>
    </div>`;
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
    setInterval(refreshStale, 1000);
    setInterval(() => {
        void call<LinkState>('LinkState').then(renderLink, (err: unknown) => logLine(`link poll: ${err}`));
    }, 2000);
    await firstPaint();
}

void boot();
