/* ROCSAR Ground Station — view types.
 *
 * These interfaces mirror the Go structs in internal/gsview/view.go,
 * internal/client/artefacts.go and cmd/gs/app.go. They replace the generated
 * wailsjs/go/models.ts, which is gone with Wails. The JSON field names are the
 * contract: a rename on either side is a silent failure, so they are written
 * out here rather than inferred.
 *
 * Absence is null, not zero — every optional field is `| null` (or `?`),
 * matching the Go pointers. The frontend switches on presence (§7.1 of
 * GUI_ARCHITECTURE.md); it never guesses from values.
 */

export interface Config {
    control_endpoint: string;
    telemetry_endpoint: string;
    http_endpoint: string;
    topic: string;
}

export interface Entry {
    name: string;
    size_bytes: number;
    modified_unix: number;
    kind: string;
    directory: boolean;
}

export interface SdrParamsPatch {
    prf_hz?: number;
    sample_rate_hz?: number;
    tx_freq_hz?: number;
    normalized_gain_tx?: number;
    normalized_gain_rx?: number;
    bandwidth_hz?: number;
    session_duration_s?: number;
    /* The burst window and the arming delay. Together with the antenna ports
     * these complete the set of params.json keys connect.cpp actually reads:
     * before they existed the console could change the radio's tuning and gain
     * but not the shape of the sweep it flies. */
    t_min_us?: number;
    t_max_us?: number;
    start_offset_s?: number;
    /* RF path names, not enums. UHD decides what is legal on this radio and
     * firmware, so these are passed through as written and the operator sees
     * whatever the program says if it refuses. */
    tx_antenna?: string;
    rx_antenna?: string;
}

export interface SystemView {
    state: string;
    uptime_s: number;
    cpu_temp_c: number;
    mocked: string[];
}

export interface PositionView {
    latitude_deg: number;
    longitude_deg: number;
    altitude_m: number;
    ground_speed_mps: number;
    course_deg: number;
    // Climbrate, m/s, positive up. Null until the estimator has a window worth
    // fitting -- about a minute after launch. Null is NOT zero: a balloon at float
    // genuinely reads ~0, and a console that cannot tell the two apart will
    // eventually call the start of every ascent a stall.
    vertical_rate_mps: number | null;
}

export interface ReceiverView {
    receiver_id: number;
    selected: boolean;
    fix_ok: boolean;
    position: PositionView | null;
    fix_age_s: number | null;
    packets_accepted: number;
    packets_rejected: number;
}

export interface AxisView {
    servo_id: number;
    manual_mode: boolean;
    current_tick: number;
    current_angle_deg: number;
    load: number | null;
    temperature_c: number | null;
    center_tick: number;
    // Whether this servo has been taught its centre. False means center_tick is
    // the built-in assumption (2048) rather than a measurement -- and the two are
    // otherwise indistinguishable on screen, which is the reason the bit exists.
    center_zeroed: boolean;
    mount_offset_deg: number;
    dir_multiplier: number;
    feedback: string;
    feedback_error: string | null;
}

export interface PicoView {
    gondola_heading_deg: number;
    target_heading_deg: number;
    heater1_on: boolean;
    heater2_on: boolean;
    imu: string;
    imu_temperature_c: number | null;
    // Tilt in degrees, null unless the IMU is present.
    gondola_roll_deg: number | null;
    gondola_pitch_deg: number | null;
    // The BNO055 calibration register verbatim: two bits per sensor, most
    // significant first (system, gyroscope, accelerometer, magnetometer), each 0
    // uncalibrated to 3 fully. One byte, not four claims.
    imu_calibration: number | null;
    // Largest linear acceleration since boot, m/s^2, gravity already removed.
    // Monotonic: this is "hardest thing that has happened", not "right now".
    imu_peak_accel_ms2: [number, number, number];
    // Increments once per shock (not once per axis). Beside the peak because a
    // monotonic maximum cannot say whether anything has happened since the last
    // frame.
    imu_peak_accel_event: number;
    antennas: AxisView[];
}

export interface AckView {
    command_sequence: number;
    success: boolean;
    error: string;
}

export interface SDRView {
    state: string;
    running: boolean;
    pid: number;
    last_log: string | null;
    last_error: string | null;
}

export interface CameraView {
    state: string;
    device: string | null;
    last_photo: string | null;
    photos_taken: number | null;
}

export interface LinkView {
    state: string;
    device: string;
    rate_kbps: number;
    priority_kbps: number;
    shaping_active: boolean;
    inactive_reason: string | null;
}

export interface View {
    sequence: number;
    generated_at: string;
    uptime_s: number;
    system: SystemView;
    gnss: ReceiverView[];
    pico: PicoView | null;
    pico_connected: boolean;
    pico_last_ack: AckView | null;
    sdr: SDRView;
    camera: CameraView;
    link: LinkView;
    healthy: boolean;
    not_healthy_reasons: string[];
}

export interface ArtefactMeta {
    name: string;
    size_bytes: number;
    kind: string;
}

export interface CommandResult {
    request_id: string;
    success: boolean;
    error: string;
    message: string;
    artefact: ArtefactMeta | null;
}

export interface LinkState {
    control_connected: boolean;
    telemetry_connected: boolean;
    last_frame_age_s: number;
    frames_received: number;
    sequence_gaps: number;
    frames_discarded: number;
    commands_in_flight: number;
    last_error: string;
}

export interface FrameEvent {
    view: View;
    gaps: number;
    restart: boolean;
    link?: LinkState;
}

export interface DownloadProgress {
    name: string;
    bytes: number;
    total: number;
    rate_kbs: number;
}

export interface DownloadDone {
    name: string;
    bytes: number;
    elapsed_s: number;
    rate_kbs: number;
    error: string;
}
