/* ROCSAR Ground Station — panel renderers.
 *
 * Every function here is pure: it takes decoded view data and returns an HTML
 * string. No DOM, no state, no side effects. That is what lets the same code
 * render the live console (main.ts drops the strings into panels) and a static
 * preview built from fixture data, which is how the instruments are checked
 * against something other than their own unit tests.
 *
 * The rules from GUI_ARCHITECTURE.md that these renderers must hold:
 *
 *   7.1  absence is not zero. A receiver with no fix, a servo with no reading
 *        and a link with no frame render as the absence, never as a zero.
 *   7.2  a held reading is not a measurement. Load and temperature are only
 *        numeric when gsview sends them (feedback MEASURED); otherwise the word
 *        says held and the gauge is empty.
 *   7.3  no satellite count, no sdr.last_output_file, degrees shown as degrees.
 *   9.4  no unit conversion. Numbers are formatted for width, never rescaled.
 *
 * Formatting lives here rather than in main.ts so the preview and the live
 * console cannot drift apart in how a value is written.
 */
import {
    barGauge,
    budgetBar,
    clockText,
    compass,
    percent,
    Ring,
    sparkline,
    stateChip,
    statTile,
    tickRing,
    durationText,
} from './widgets';
import type { View, LinkState, LinkView, AxisView } from './models';
import type { ChipKind } from './widgets';

export function esc(s: string): string {
    return s.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;');
}

export function fmtDeg(v: number): string {
    return `${v.toFixed(2)}°`;
}

export function fmtUptime(s: number): string {
    const h = Math.floor(s / 3600);
    const m = Math.floor((s % 3600) / 60);
    const sec = Math.floor(s % 60);
    return `${h}h${m.toString().padStart(2, '0')}m${sec.toString().padStart(2, '0')}s`;
}

export function fmtBytes(n: number): string {
    if (n < 1024) {
        return `${n} B`;
    }
    if (n < 1024 * 1024) {
        return `${(n / 1024).toFixed(1)} KiB`;
    }
    return `${(n / 1024 / 1024).toFixed(1)} MiB`;
}

export function fmtAge(s: number): string {
    if (s < 1.5) {
        return 'just now';
    }
    if (s < 90) {
        return `${s.toFixed(1)}s ago`;
    }
    return `${(s / 60).toFixed(1)}m ago`;
}

/* ------------------------------------------------------------------ */
/* system                                                             */
/* ------------------------------------------------------------------ */

export function renderSystem(v: View): string {
    const s = v.system;
    const mocked =
        s.mocked && s.mocked.length > 0
            ? `<div class="banner warn">SIMULATED: ${s.mocked.map(esc).join(', ')} — fabricated, not measured</div>`
            : '';
    const stateKind: ChipKind = s.state === 'READY' ? 'ok' : 'warn';
    return `${mocked}
        <div class="row"><span>state</span>${stateChip(s.state, stateKind)}</div>
        <div class="row"><span>uptime</span><b>${fmtUptime(s.uptime_s)}</b></div>
        <div class="row"><span>cpu</span><b>${s.cpu_temp_c.toFixed(0)} °C</b></div>`;
}

/* ------------------------------------------------------------------ */
/* gnss                                                               */
/* ------------------------------------------------------------------ */

function gnssTile(g: View['gnss'][number]): string {
    const selected = g.selected ? stateChip('selected', 'info') : '';
    const fix = g.position === null || g.position === undefined;
    const fixChip = fix ? stateChip('no fix', 'bad') : stateChip('fix', 'ok');

    const pos = fix
        ? '<i>no fix — not a position</i>'
        : `${g.position!.latitude_deg.toFixed(4)}, ${g.position!.longitude_deg.toFixed(4)} · alt ${g.position!.altitude_m.toFixed(1)} m`;

    const age = g.fix_age_s === null || g.fix_age_s === undefined ? 'never' : fmtAge(g.fix_age_s);

    // Climbrate, and the em dash for "not derivable yet".
    //
    // The distinction from zero is the entire point of the field. The estimator
    // needs about a minute of window before it will fit anything, so for the first
    // minute of a flight there is no rate at all. Printing 0.00 m/s there would
    // assert that the balloon is at float, which is the one thing it definitively
    // is not. A balloon that really is at float prints a small number from the
    // same formatter.
    const vr = g.position?.vertical_rate_mps;
    const climbChip = vr === null || vr === undefined
        ? stateChip('climb —', 'dim')
        : stateChip(`climb ${vr >= 0 ? '+' : ''}${vr.toFixed(2)} m/s`, Math.abs(vr) < 0.5 ? 'info' : 'ok');

    // A receiver that has decoded no packets at all is not "0% accepted"; it is
    // absent, and the bar says so (7.1).
    const total = g.packets_accepted + g.packets_rejected;
    const ratio = barGauge({
        label: 'accepted',
        value: total > 0 ? g.packets_accepted : null,
        max: total,
        unit: '',
        absentText: 'no packets',
    });

    return `<div class="w-card">
      <div class="w-card-head"><b>gnss ${g.receiver_id}</b> ${selected} ${fixChip}</div>
      <div class="row sub"><span></span><b>${pos}</b></div>
      <div class="row sub"><span></span>${climbChip}</div>
      <div class="row sub"><span></span><span>fix ${age} · ok ${g.packets_accepted} / rej ${g.packets_rejected}</span></div>
      ${ratio}
    </div>`;
}

export function renderGnss(v: View): string {
    if (v.gnss.length === 0) {
        return '<div class="row"><i>no receivers reported</i></div>';
    }
    return v.gnss.map(gnssTile).join('');
}

/* ------------------------------------------------------------------ */
/* flight controller                                                  */
/* ------------------------------------------------------------------ */

function feedbackKind(feedback: string): ChipKind {
    if (feedback === 'MEASURED') {
        return 'ok';
    }
    if (feedback === 'HELD') {
        return 'warn';
    }
    return 'dim';
}

/* subsystemKind maps a SubsystemState string onto a chip colour.
 *
 * The wire carries the domain enum's own spelling and nothing else:
 * DISCONNECTED, READY, BUSY, ERROR, UNSPECIFIED. Two comparisons in this file
 * tested for 'RUNNING' and 'ACTIVE', which are not in that set and which no
 * producer anywhere emits -- so the SDR and link chips rendered permanently
 * 'dim', including while an acquisition was running and while the link was
 * healthy. A chip that can never light up is worse than no chip: it looks like a
 * deliberate judgement about the state.
 *
 * READY and BUSY are both 'ok'. For the SDR, BUSY is an acquisition in flight;
 * for the link, BUSY is shaping in force with a caveat the reason line spells
 * out. Neither is a fault, and colouring them 'dim' would say otherwise.
 *
 * Anything unrecognised is 'bad' rather than a healthy colour. A state string
 * this build does not know is a client and server that disagree about the
 * schema, and it must not read as fine. */
function subsystemKind(state: string): ChipKind {
    switch (state) {
        case 'READY':
        case 'BUSY':
            return 'ok';
        default:
            return 'bad';
    }
}

function axisCard(a: AxisView): string {
    const fault =
        a.feedback_error !== null && a.feedback_error !== undefined
            ? `<div class="row">${stateChip(`FAULT ${a.feedback_error}`, 'bad', 'ST3215 status bits: overheat, overload, undervoltage')}</div>`
            : '';

    // center_tick alone cannot say whether it was measured: the servo holds its
    // own zero now, so an axis that was never taught reports the same 2048 as
    // one that was, and presenting that as "centred" is a lie an operator would
    // act on.
    const centre = a.center_zeroed
        ? stateChip(`centre ${a.center_tick} taught`, 'ok')
        : stateChip(`centre UNTAUGHT (assumed ${a.center_tick})`, 'warn', 'the servo has never been taught its centre');

    const load = barGauge({ label: 'load', value: a.load, max: 100, unit: '%' });
    const temp =
        a.temperature_c === null || a.temperature_c === undefined
            ? '<div class="row sub"><span>temp</span><i>held</i></div>'
            : `<div class="row sub"><span>temp</span><b>${a.temperature_c} °C</b></div>`;

    return `<div class="w-card servo">
      <div class="servo-top">
        ${tickRing(a.current_tick)}
        <div class="servo-facts">
          <div class="servo-title">servo ${a.servo_id} ${stateChip(a.feedback, feedbackKind(a.feedback))}</div>
          <div class="servo-line">tick <b>${a.current_tick}</b> · <b>${fmtDeg(a.current_angle_deg)}</b></div>
          <div class="servo-line">${centre}</div>
        </div>
      </div>
      ${load}
      ${temp}
      ${fault}
      <div class="servo-actions">
        <button data-motion data-zero-servo="${a.servo_id}" title="teach the servo the tick it is already at; the axis does not move">zero</button>
        <button data-motion data-stop-servo="${a.servo_id}">stop</button>
      </div>
    </div>`;
}

// imuCalibrationChip decodes the BNO055's calibration register.
//
// Layout is register 0x35 verbatim: two bits per sensor, most significant first
// -- system, gyroscope, accelerometer, magnetometer -- each 0 uncalibrated
// through 3 fully calibrated.
//
// The verdict is the magnetometer, and only the magnetometer. The system and
// gyroscope bits track the fusion's internal state; what decides whether a
// heading means anything is whether the magnetometer has been calibrated, since
// an uncalibrated one produces a bearing that is confidently wrong.
function imuCalibrationChip(raw: number | null | undefined): string {
    if (raw === null || raw === undefined) {
        return '<div class="row sub"><span>imu calib</span><i>held</i></div>';
    }
    const sensors = ['sys', 'gyro', 'accel', 'mag'] as const;
    const parts = sensors.map((name, i) => `${name} ${(raw >> (6 - 2 * i)) & 3}`);
    const mag = (raw >> 0) & 3;
    return (
        '<div class="row sub"><span>imu calib</span>' +
        `<b>${parts.join(' · ')}</b> ${stateChip(mag === 3 ? 'mag calibrated' : 'mag UNCALIBRATED', mag === 3 ? 'ok' : 'warn')}` +
        '</div>'
    );
}


export function renderPico(v: View): string {
    if (v.pico === null || v.pico === undefined) {
        return '<div class="row"><span>pico</span><b>no data</b></div>';
    }
    const p = v.pico;
    const held = p.imu === 'HELD';

    const conn = v.pico_connected ? stateChip('connected', 'ok') : stateChip('NOT connected', 'bad');
    const imu = stateChip(`imu ${p.imu}`, p.imu === 'MEASURED' ? 'ok' : held ? 'warn' : 'dim');
    const heat1 = stateChip(`heater 1 ${p.heater1_on ? 'on' : 'off'}`, p.heater1_on ? 'warn' : 'dim');
    const heat2 = stateChip(`heater 2 ${p.heater2_on ? 'on' : 'off'}`, p.heater2_on ? 'warn' : 'dim');
    const ack =
        v.pico_last_ack === null || v.pico_last_ack === undefined
            ? stateChip('no ack', 'dim')
            : stateChip(
                  `ack #${v.pico_last_ack.command_sequence} ${v.pico_last_ack.success ? 'ok' : v.pico_last_ack.error}`,
                  v.pico_last_ack.success ? 'ok' : 'bad',
              );

    const imuTemp =
        p.imu_temperature_c === null || p.imu_temperature_c === undefined
            ? '<div class="row sub"><span>imu temp</span><i>held</i></div>'
            : `<div class="row sub"><span>imu temp</span><b>${p.imu_temperature_c.toFixed(1)} °C</b></div>`;

    // Tilt. Its job is to be a confidence signal on the heading rather than
    // attitude in its own right: the BNO055 tilt-compensates its fusion using its
    // accelerometer, and a gondola on a 10-40 m tether swings at roughly
    // 0.1 Hz, so sway corrupts the estimate the heading correction depends on.
    // Erratic tilt is the tell that the bearing has gone bad.
    const imuTilt =
        p.gondola_roll_deg === null || p.gondola_roll_deg === undefined
            ? '<div class="row sub"><span>imu tilt</span><i>held</i></div>'
            : `<div class="row sub"><span>imu tilt</span><b>roll ${p.gondola_roll_deg.toFixed(1)}° · pitch ${(p.gondola_pitch_deg ?? 0).toFixed(1)}°</b></div>`;

    // Calibration, decoded rather than shown as a byte.
    //
    // The wire carries register 0x35 verbatim -- two bits per sensor, system
    // first -- and "sys 3 gyro 3 accel 2 mag 1" is the only form anyone can act
    // on. The magnetometer is the one that decides whether a heading is
    // trustworthy over a 2-24 hour flight, and it is invisible in a raw byte.
    const imuCalib = imuCalibrationChip(p.imu_calibration);

    // The peak never falls, so it is labelled as the flight's hardest moment
    // rather than a live reading, and the event counter beside it is what says
    // whether anything has changed since the last frame.
    //
    // Shown even when the IMU is absent. The peak of a sensor that has since
    // stopped answering is exactly what an operator wants after the fact, and
    // hiding it would erase the record of whatever stopped it.
    const peak = p.imu_peak_accel_ms2 ?? [0, 0, 0];
    const peakMag = Math.max(Math.abs(peak[0]), Math.abs(peak[1]), Math.abs(peak[2]));
    const events = p.imu_peak_accel_event ?? 0;
    const imuPeak =
        `<div class="row sub"><span>accel peak</span><b>${peakMag.toFixed(1)} m/s²</b>` +
        ` <span class="hint">since boot · ${events} event${events === 1 ? '' : 's'}</span></div>`;

    const axes = (p.antennas || []).map(axisCard).join('');

    return `<div class="pico-head">${conn} ${imu} ${heat1} ${heat2} ${ack}</div>
        <div class="compass-slot">${compass({ currentDeg: p.gondola_heading_deg, targetDeg: p.target_heading_deg, held })}</div>
        ${imuTemp}
        ${imuTilt}
        ${imuCalib}
        ${imuPeak}
        <div class="servo-grid">${axes}</div>`;
}

/* ------------------------------------------------------------------ */
/* camera · sdr · link                                                */
/* ------------------------------------------------------------------ */

export function renderCameraSdrLink(v: View, txHistory: number[] = []): string {
    const c = v.camera;
    const photos = c.photos_taken === null || c.photos_taken === undefined ? '—' : `${c.photos_taken}`;
    const photosChip = stateChip(`${photos} photo(s)`, 'dim');
    const device = c.device === null || c.device === undefined ? '' : ` ${esc(c.device)}`;
    const last = c.last_photo ? ` <span class="hint">last ${esc(c.last_photo)}</span>` : '';

    const s = v.sdr;
    /* The PID, not `running`. This rendered "running pid true" for as long as the
     * field existed, because the boolean and the pid sit next to each other in
     * the view and only one of them is a number. An operator checking whether an
     * acquisition is theirs was reading the word "true". */
    const pid = s.running ? ` running pid ${s.pid}` : '';
    const sdrErr = s.last_error ? `<div class="row sub"><span>sdr error</span><b class="err">${esc(s.last_error)}</b></div>` : '';
    const sdrLog = s.last_log ? `<div class="row sub"><span>sdr log</span><span class="hint">${esc(s.last_log)}</span></div>` : '';

    const l = v.link;
    const shaping = l.shaping_active
        ? stateChip('shaping on', 'info')
        : stateChip(`shaping off${l.inactive_reason ? `: ${l.inactive_reason}` : ''}`, 'dim');

    return `<div class="w-card-head"><b>camera</b> ${stateChip(c.state, subsystemKind(c.state))}${device} ${photosChip}${last}</div>
        <div class="w-card-head"><b>sdr</b> ${stateChip(s.state, subsystemKind(s.state))}${pid}</div>
        ${sdrErr}${sdrLog}
        <div class="w-card-head"><b>link</b> ${stateChip(l.state, subsystemKind(l.state))} ${esc(l.device)} ${l.rate_kbps} kbit/s ${shaping}</div>
        ${renderBudget(l, txHistory)}
        <div class="hint">priority ${l.priority_kbps} kbit/s</div>`;
}

/* The budget bar plus a short history of tx. The sparkline is what shows a
 * burst after it has ended; the bar alone is only ever "now". */
export function renderBudget(l: LinkView, txHistory: number[]): string {
    return `<div class="budget">
      ${budgetBar({ capKbps: l.rate_kbps, measuredKbps: l.measured_tx_kbps, enforced: l.shaping_active })}
      <div class="budget-history">
        <div class="w-tile-label">tx history</div>
        ${sparkline({ values: txHistory, label: 'measured tx, kbit/s', min: 0 })}
      </div>
    </div>`;
}

/* Tiles for the persistent status strip. Kept separate from the bar because the
 * strip is rendered from the link-connection event and the bar from the frame;
 * both draw the same LinkView fields but are refreshed at different times. */
export function renderBudgetTiles(l: LinkView | null): string {
    if (l === null) {
        return '';
    }
    const rate = (v: number | null): string => (v === null ? 'no reading' : `${v}`);
    const kind = (v: number | null): ChipKind => (v === null ? 'dim' : v > l.rate_kbps ? 'bad' : 'ok');
    return (
        statTile({ label: 'cap', value: `${l.rate_kbps} kbit/s` }) +
        statTile({ label: 'tx', value: rate(l.measured_tx_kbps), sub: 'kbit/s', kind: kind(l.measured_tx_kbps) }) +
        statTile({ label: 'rx', value: rate(l.measured_rx_kbps), sub: 'kbit/s', kind: kind(l.measured_rx_kbps) })
    );
}

/* ------------------------------------------------------------------ */
/* link status strip                                                  */
/* ------------------------------------------------------------------ */

export function renderLinkLine(l: LinkState, ageHistory: number[]): string {
    const dot = (ok: boolean): ChipKind => (ok ? 'ok' : 'bad');
    const ageFresh = l.frames_received > 0 && l.last_frame_age_s <= 3;
    return (
        stateChip((l.control_connected ? '● ' : '○ ') + 'control', dot(l.control_connected)) +
        stateChip((l.telemetry_connected ? '● ' : '○ ') + 'telemetry', dot(l.telemetry_connected)) +
        statTile({ label: 'frame age', value: l.frames_received === 0 ? 'never' : fmtAge(l.last_frame_age_s), kind: ageFresh ? 'ok' : 'bad' }) +
        statTile({ label: 'frames', value: String(l.frames_received) }) +
        statTile({ label: 'gaps', value: String(l.sequence_gaps), kind: l.sequence_gaps > 0 ? 'warn' : undefined }) +
        statTile({ label: 'dropped', value: String(l.frames_discarded) }) +
        statTile({ label: 'in flight', value: String(l.commands_in_flight) }) +
        `<div class="w-tile"><div class="w-tile-label">age history</div>${sparkline({ values: ageHistory, label: 'frame age, last two minutes', min: 0 })}</div>` +
        (l.last_error ? stateChip(l.last_error, 'bad') : '')
    );
}

/* ------------------------------------------------------------------ */
/* attention summary                                                  */
/* ------------------------------------------------------------------ */

export interface AttentionItem {
    kind: ChipKind;
    text: string;
    where: string;
}

/* collectAttention gathers everything wrong, in one deterministic order, so the
 * answer to "is anything wrong" is one strip rather than a scan of eight panels.
 * The order is severity-first and stable: a summary that reorders itself between
 * frames is worse than no summary.
 *
 * Every text comes from the wire. Nothing here infers a fault from a value. */
export function collectAttention(v: View, link: LinkState | null): AttentionItem[] {
    const out: AttentionItem[] = [];

    if (link !== null && (link.frames_received === 0 || !link.telemetry_connected)) {
        out.push({ kind: 'bad', text: 'telemetry link has no frames', where: 'status' });
    }
    for (const r of v.not_healthy_reasons || []) {
        out.push({ kind: 'bad', text: r, where: 'health' });
    }
    for (const m of v.system.mocked || []) {
        out.push({ kind: 'warn', text: `${m} is simulated`, where: 'system' });
    }
    if (v.pico === null || v.pico === undefined) {
        out.push({ kind: 'bad', text: 'flight controller has no data', where: 'flight controller' });
    } else {
        for (const a of v.pico.antennas || []) {
            if (a.feedback_error !== null && a.feedback_error !== undefined) {
                out.push({ kind: 'bad', text: `servo ${a.servo_id} fault: ${a.feedback_error}`, where: 'flight controller' });
            }
            if (!a.center_zeroed) {
                out.push({ kind: 'warn', text: `servo ${a.servo_id} centre UNTAUGHT (assumed ${a.center_tick})`, where: 'flight controller' });
            }
        }
    }
    if (!v.pico_connected) {
        out.push({ kind: 'bad', text: 'pico not connected', where: 'flight controller' });
    }
    for (const g of v.gnss) {
        if (!g.fix_ok) {
            out.push({ kind: 'warn', text: `gnss ${g.receiver_id} has no fix`, where: 'gnss' });
        }
    }
    if (v.link.inactive_reason) {
        out.push({ kind: 'warn', text: `link shaping inactive: ${v.link.inactive_reason}`, where: 'camera · sdr · link' });
    }
    if (link !== null && link.last_error) {
        out.push({ kind: 'bad', text: link.last_error, where: 'status' });
    }
    return out;
}

export function renderAttention(v: View, link: LinkState | null): string {
    const items = collectAttention(v, link);
    if (items.length === 0) {
        return stateChip('all clear', 'ok');
    }
    return (
        stateChip(`${items.length} to note`, items.some((i) => i.kind === 'bad') ? 'bad' : 'warn') +
        items
            .map(
                (i) =>
                    stateChip(i.text, i.kind, `see ${i.where}`) + `<span class="hint attention-where">${esc(i.where)}</span>`,
            )
            .join('')
    );
}

/* ------------------------------------------------------------------ */
/* download                                                           */
/* ------------------------------------------------------------------ */

/** renderDownload is the progress bar H1 requires: a bar, a rate, and a
 *  wall-clock finish time. A duration alone ("ETA 3721s") is a number an
 *  operator has to do arithmetic on while waiting an hour. */
export function renderDownload(name: string, bytes: number, total: number, rateKbs: number): string {
    const known = total > 0;
    const p = known ? percent(bytes, total) : 0;
    const remaining = known && rateKbs > 0.1 ? (total - bytes) / 1024 / rateKbs : Number.NaN;
    const eta = Number.isFinite(remaining)
        ? `ETA ${clockText(remaining)} (${durationText(remaining)})`
        : 'ETA unknown';
    const size = known ? `${fmtBytes(bytes)} / ${fmtBytes(total)}` : fmtBytes(bytes);
    return `<div class="dl-name">${esc(name)}</div>
      <div class="w-bar-track dl-track"><span class="w-bar-fill" style="width:${p.toFixed(1)}%"></span></div>
      <div class="dl-meta">${p.toFixed(1)}% · ${size} · ${rateKbs.toFixed(1)} KiB/s · ${eta}</div>`;
}

/* ringForHistory is re-exported so main.ts and the preview share one bound. */
export function telemetryAgeRing(): Ring {
    return new Ring(120);
}
