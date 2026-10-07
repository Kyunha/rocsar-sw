/* Render the panels from fixture data to a static HTML file.
 *
 * This is a development tool, not a test: it exists so the instruments can be
 * looked at, which unit tests cannot do. It composes the same renderers the
 * live console uses (panels.ts) and the same stylesheet, so what it draws is
 * what an operator sees, from a View that exercises the awkward cases -- a held
 * servo, an untaught centre, a fault, a receiver with no fix, a mocked
 * subsystem, a link carrying no frames.
 *
 * Build and run it with `npm run fixture`, which bundles this with esbuild and
 * writes /tmp/gs-preview.html. Open that in a browser (or screenshot it
 * headless) to check the layout.
 */
import { readFileSync, writeFileSync } from 'node:fs';
import { join } from 'node:path';

import {
    renderAttention,
    renderCameraSdrLink,
    renderDownload,
    renderGnss,
    renderLinkLine,
    renderPico,
    renderSystem,
} from '../src/panels';
import { knob } from '../src/widgets';
import type { View, LinkState } from '../src/models';

/* Run from cmd/gs/frontend (npm sets cwd there), so the stylesheet is at a
 * known relative path. */
const css = readFileSync(process.env.GS_CSS ?? join(process.cwd(), 'src', 'style.css'), 'utf8');

const view: View = {
    sequence: 4421,
    generated_at: '2026-10-06T23:40:00Z',
    uptime_s: 7712,
    system: { state: 'READY', uptime_s: 7712, cpu_temp_c: 54, mocked: ['sdr'] },
    gnss: [
        {
            receiver_id: 1,
            selected: true,
            fix_ok: true,
            position: {
                latitude_deg: 61.123456,
                longitude_deg: 8.56789,
                altitude_m: 412.3,
                ground_speed_mps: 11.4,
                course_deg: 271.5,
                vertical_rate_mps: 4.82,
            },
            fix_age_s: 0.2,
            packets_accepted: 1841,
            packets_rejected: 17,
        },
        {
            receiver_id: 2,
            selected: false,
            fix_ok: false,
            position: null,
            fix_age_s: null,
            packets_accepted: 0,
            packets_rejected: 0,
        },
    ],
    pico: {
        gondola_heading_deg: 123.4,
        target_heading_deg: 130.0,
        heater1_on: true,
        heater2_on: false,
        imu: 'MEASURED',
        imu_temperature_c: 31.5,
        gondola_roll_deg: 4.2,
        gondola_pitch_deg: -1.8,
        imu_calibration: 0b11_11_10_01,
        imu_peak_accel_ms2: [1.2, -14.6, 3.1],
        imu_peak_accel_event: 3,
        antennas: [
            {
                servo_id: 1,
                manual_mode: false,
                current_tick: 2610,
                current_angle_deg: 49.39,
                load: 37,
                temperature_c: 42,
                center_tick: 2048,
                center_zeroed: true,
                mount_offset_deg: 0,
                dir_multiplier: 1,
                feedback: 'MEASURED',
                feedback_error: null,
            },
            {
                servo_id: 2,
                manual_mode: true,
                current_tick: 2048,
                current_angle_deg: 0,
                load: null,
                temperature_c: null,
                center_tick: 2048,
                center_zeroed: false,
                mount_offset_deg: -3,
                dir_multiplier: -1,
                feedback: 'HELD',
                feedback_error: 'overload+overheat',
            },
        ],
    },
    pico_connected: true,
    pico_last_ack: { command_sequence: 91, success: true, error: '' },
    sdr: { state: 'RUNNING', running: true, pid: 2041, last_log: '/var/log/connect.log', last_error: null },
    camera: { state: 'READY', device: '/dev/video0', last_photo: 'IMG_0042.jpg', photos_taken: 43 },
    link: {
        state: 'ACTIVE',
        device: 'tun0',
        rate_kbps: 115,
        priority_kbps: 96,
        shaping_active: false,
        inactive_reason: 'bulk queue empty',
    },
    healthy: false,
    not_healthy_reasons: ['bulk link shaping inactive'],
};

const link: LinkState = {
    control_connected: true,
    telemetry_connected: true,
    last_frame_age_s: 0.4,
    frames_received: 4421,
    sequence_gaps: 2,
    frames_discarded: 1,
    commands_in_flight: 0,
    last_error: '',
};

const ages = Array.from({ length: 60 }, (_, i) => 0.2 + Math.abs(Math.sin(i / 7)) * 1.6);

const knobs = {
    heading: knob({ forId: 'in-heading', label: 'target heading', min: 0, max: 359, step: 1, value: 130, unit: '°' }),
    jog: knob({ forId: 'in-jog-tick', label: 'jog tick', min: 0, max: 4095, step: 1, value: 2048, unit: 'tick' }),
    mount: knob({ forId: 'in-mount-deg', label: 'mount offset', min: -180, max: 180, step: 1, value: -3, unit: '°' }),
};

const html = `<!doctype html>
<html><head><meta charset="utf-8"><title>ROCSAR fixture</title><style>${css}</style></head>
<body><div id="app">
  <header>
    <h1>ROCSAR Ground Station</h1>
    <div class="conn">
      <input size="22" value="tcp://127.0.0.1:5555"><input size="22" value="tcp://127.0.0.1:5556"><input size="20" value="http://127.0.0.1:8080">
      <button>connect</button><button>disconnect</button>
    </div>
  </header>
  <div id="statusbar">
    <div id="linkline" class="status-link">${renderLinkLine(link, ages)}</div>
    <div id="meta" class="status-meta">seq 4421 · uptime 128m</div>
  </div>
  <div id="attention" class="attention">${renderAttention(view, link)}</div>
  <div id="layout">
    <div class="zone-main">
      <section class="p-pico motion-zone">
        <h2>flight controller</h2>
        <div id="pico">${renderPico(view)}</div>
        <div class="controls">
          <div class="ctl"><span class="ctl-label">heading (deg)</span>${knobs.heading}
            <input size="5" value="130"><button data-motion>set heading</button><button>status</button></div>
          <div class="ctl"><span class="ctl-label">jog (tick 0–4095)</span>${knobs.jog}
            <input size="2" value="1"><input size="5" value="2048"><button data-motion>jog</button><button data-motion>zero both</button></div>
          <div class="ctl"><span class="ctl-label">mount offset (deg)</span>${knobs.mount}
            <input size="2" value="1"><input size="5" value="-3"><button data-motion>mount</button></div>
          <div class="ctl"><span class="ctl-label">direction</span>
            <input size="2" value="1"><input size="3" value="+1"><button data-motion>direction</button></div>
          <div class="ctl"><span class="ctl-label">heater</span>
            <input size="2" value="1"><input size="4" value="on"><button data-motion>heater</button></div>
          <div class="ctl ctl-stop"><button data-motion class="stop">STOP ALL</button></div>
        </div>
      </section>
      <section class="p-map">
        <h2>map <span class="hint">click to point both antennas</span></h2>
        <div style="height:300px;border:1px dashed #2a2e37;border-radius:6px;display:flex;align-items:center;justify-content:center;color:#8b909b">map — not rendered in the fixture</div>
        <div id="position">Lat: 61.1234°, Lon: 8.5679°, Alt: 412.3 m</div>
      </section>
    </div>
    <div class="zone-rail">
      <section class="p-sys"><h2>system</h2><div id="sys">${renderSystem(view)}</div></section>
      <section class="p-gnss"><h2>gnss</h2><div id="gnss">${renderGnss(view)}</div></section>
      <section class="p-csl"><h2>camera · sdr · link</h2><div id="csl">${renderCameraSdrLink(view)}</div></section>
    </div>
    <div class="zone-bottom">
      <details class="panel-fold" open><summary>artefacts /</summary><section class="p-art">
        <div class="row"><span>IMG_0042.jpg</span><span>camera 3.1 MiB</span> <button>fetch</button> <button>preview</button></div>
        <div class="row"><span>sweep_2026.bin</span><span>sdr 30.0 MiB</span> <button>fetch</button></div>
      </section></details>
      <details class="panel-fold" open><summary>download</summary><section class="p-art">
        ${renderDownload('sweep_2026_10_06.bin', 4_200_000, 31_457_280, 8.1)}
      </section></details>
      <details class="panel-fold" open><summary>log</summary><section class="p-log"><div id="log">
        <div>gs version 0.4.2</div><div>connect requested</div><div>params refreshed from OBC</div>
      </div></section></details>
    </div>
  </div>
</div></body></html>`;

const out = process.argv[2] ?? '/tmp/gs-preview.html';
writeFileSync(out, html);
console.log(`wrote ${out} (${html.length} bytes)`);
