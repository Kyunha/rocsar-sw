/* ROCSAR Ground Station — Offline Map.
 *
 * Display GNSS position on a fixed offline map using Leaflet.
 * Tiles are not required: the map shows a position marker and trail
 * over a background representing the operating area (Portugal/Northern Europe),
 * fully bundled into the Go binary via Wails — zero network at runtime.
 *
 * Per GUI_ARCHITECTURE.md: no protobuf in the browser, degrees displayed
 * as-is, absence handled by internal/gsview.
 */

import * as L from 'leaflet';
import { EventsOn } from '../wailsjs/runtime/runtime';

const MAP_ZOOM = 5;
const MAX_ZOOM = 6;
const TRAIL_MAX = 30;

let map: L.Map | null = null;
let marker: L.Marker | null = null;
let trailFeature: L.Polyline | null = null;
const trail: L.LatLng[] = [];

/* Telemetry frame shape — only the fields needed for the map; in sync with
 * internal/gsview view struct, no protobuf, no socket imports. */
interface TelemetryFrameView {
    gnss: Array<{
        latitude_deg: number;
        longitude_deg: number;
        altitude_m: number;
        ground_speed_mps: number;
        course_deg: number;
        fix_ok: boolean;
        fix_age_s: number | null;
    }>;
}

/* Initialise the Leaflet map, marker, and event subscription.
 * No tile layer: background color shows the operating area;
 * all map assets are bundled into the Go binary at wails build time. */
function initMap(): void {
    map = L.map('map', {
        center: [39, -8],   // Portugal approximate centre
        zoom: MAP_ZOOM,
        maxZoom: MAX_ZOOM,
        crs: L.CRS.Simple,
    });

    /* Background: give the map a color/pattern indicating the operating area.
     * The map div has a stylesheet rule; we just ensure the map knows its bounds. */
    map.setMaxBounds([[90, -20], [10, -10]]);  // rough Portugal/NW Europe bounds

    /* Current position marker — red when having a fix, gray when stale */
    marker = L.marker([39, -8], {
        icon: L.icon({
            iconUrl: 'leaflet/images/marker-icon.png',
            iconSize: [25, 41],
            iconAnchor: [12, 41],
        }),
    }).addTo(map);
    marker.bindPopup('Current Position').openPopup();

    /* Subscribe to telemetry frames to update position + trail */
    EventsOn('telemetry:frame', (ev: unknown) => {
        const frame = ev as TelemetryFrameView;
        updateFromFrame(frame);
    });
}

/* Update marker position, trail, and readout from a decoded telemetry frame. */
function updateFromFrame(frame: TelemetryFrameView): void {
    if (frame.gnss === undefined || frame.gnss.length === 0) {
        return;
    }
    const g = frame.gnss[frame.gnss.length - 1];
    if (g.latitude_deg === undefined || g.longitude_deg === undefined) {
        return;
    }

    const lat = g.latitude_deg;
    const lon = g.longitude_deg;
    const alt = g.altitude_m;
    const fixOk = g.fix_ok !== false;

    /* Position readout below the map */
    const posEl = document.querySelector<HTMLElement>('#position')!;
    posEl.textContent = fixOk
        ? `Lat: ${lat.toFixed(4)}°, Lon: ${lon.toFixed(4)}°, Alt: ${alt !== undefined ? alt.toFixed(1) : '—'} m`
        : 'Position: no fix';

    /* Update marker */
    if (map) {
        const iconUrl = fixOk
            ? 'leaflet/images/marker-icon.png'
            : 'leaflet/images/marker-shadow.png';
        marker!.setIcon(L.icon({ iconUrl, iconSize: [25, 41], iconAnchor: [12, 41] }));
        marker!.setLatLng(L.latLng(lat, lon));
    }

    /* Maintain trail (last TRAIL_MAX points = ~30 s at 1 Hz) */
    trail.push(L.latLng(lat, lon));
    if (trail.length > TRAIL_MAX) {
        trail.shift();
    }
    if (trailFeature !== null && map) {
        trailFeature.setLatLngs(trail);
    }
    /* Create the polyline on the first point so bounds are known */
    if (trailFeature === null && map && trail.length >= 2) {
        trailFeature = L.polyline(trail, {
            color: '#6aa9ff',
            weight: 2,
            opacity: 0.8,
            dashArray: '5, 5',
        }).addTo(map);
        /* Fit map to the trail after a few points */
        if (trail.length >= 3) {
            map.fitBounds(trailFeature.getBounds());
        }
    }
}

export { initMap };