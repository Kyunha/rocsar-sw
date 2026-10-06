/* ROCSAR Ground Station — Offline Map.
 *
 * Displays GNSS position on a low-detail offline basemap rendered by MapLibre
 * GL JS from Natural Earth 50m GeoJSON: ocean, land, lakes, rivers, mountain
 * ranges and named summits. Every asset is bundled into the Go binary and
 * served locally (pre-gzipped, see cmd/gs/bridge.go) — zero network at runtime.
 *
 * Why GeoJSON and not tiles: GUI_ARCHITECTURE.md 1.2. The operating map needs
 * geographic context at a regional scale, not a street basemap. Natural Earth
 * is public domain, generalised for exactly this scale, and its vector layers
 * drop straight into a MapLibre GeoJSON source with no tile protocol, no
 * archive format and no tile server. See scripts/fetch-basemap.py for how the
 * assets are produced.
 *
 * Per GUI_ARCHITECTURE.md: no protobuf in the browser, degrees displayed
 * as-is (section 7.3), absence handled by internal/gsview. The map is a primary
 * input surface (1.2): a click reports its coordinates through onMapClick.
 */

import maplibregl from 'maplibre-gl';
import { on } from './bridge';
import type { FrameEvent, View } from './models';

const GEOJSON = '/geojson';

/* Default view before the first fix: Porto, the operating area. On the first
 * valid fix the map jumps to the gondola, so the default only matters for the
 * seconds before telemetry arrives. */
const DEFAULT_CENTER: [number, number] = [-8.61, 41.15];
const DEFAULT_ZOOM = 8;
const FIX_ZOOM = 12;

const TRAIL_MAX = 30;

/* Heading line length in degrees. At zoom 14, ~0.002° ≈ 200 m — long enough to
 * read the bearing, short enough to stay on the gondola. */
const HEADING_LEN_DEG = 0.002;

const EMPTY_LINE: GeoJSON.Feature<GeoJSON.LineString> = {
    type: 'Feature',
    properties: {},
    geometry: { type: 'LineString', coordinates: [] },
};

let map: maplibregl.Map | null = null;
let marker: maplibregl.Marker | null = null;
let trailSource: maplibregl.GeoJSONSource | null = null;
let headingSource: maplibregl.GeoJSONSource | null = null;
const trail: [number, number][] = [];
let centeredOnFix = false;

/* Click-to-point callback. Set by main.ts when the command path is ready.
 * Receives latitude and longitude in degrees, unconverted (section 7.3). */
let clickHandler: ((lat: number, lon: number) => void) | null = null;

export function setMapClickHandler(fn: ((lat: number, lon: number) => void) | null): void {
    clickHandler = fn;
}

/* The dark basemap. Palette matches the console (frontend/src/style.css):
 * water is the one saturated hue, land and relief stay near the page
 * background, and the gondola marker is the brightest thing on the map so the
 * eye finds it without a legend. */
const darkStyle: maplibregl.StyleSpecification = {
    version: 8,
    sources: {
        ocean: { type: 'geojson', data: `${GEOJSON}/ne_50m_ocean.json` },
        land: { type: 'geojson', data: `${GEOJSON}/ne_50m_land.json` },
        lakes: { type: 'geojson', data: `${GEOJSON}/ne_50m_lakes.json` },
        rivers: { type: 'geojson', data: `${GEOJSON}/ne_50m_rivers_lake_centerlines.json` },
        ranges: { type: 'geojson', data: `${GEOJSON}/ne_50m_mountain_ranges.json` },
        peaks: { type: 'geojson', data: `${GEOJSON}/ne_50m_geography_regions_elevation_points.json` },
        trail: { type: 'geojson', data: EMPTY_LINE },
        heading: { type: 'geojson', data: EMPTY_LINE },
    },
    layers: [
        { id: 'background', type: 'background', paint: { 'background-color': '#121418' } },
        { id: 'ocean', type: 'fill', source: 'ocean', paint: { 'fill-color': '#14202c' } },
        { id: 'land', type: 'fill', source: 'land', paint: { 'fill-color': '#1a1d23' } },
        {
            id: 'ranges',
            type: 'fill',
            source: 'ranges',
            paint: { 'fill-color': '#2b2118', 'fill-opacity': 0.5 },
        },
        {
            id: 'ranges-outline',
            type: 'line',
            source: 'ranges',
            paint: { 'line-color': '#4a3a28', 'line-width': 0.8, 'line-opacity': 0.9 },
        },
        { id: 'lakes', type: 'fill', source: 'lakes', paint: { 'fill-color': '#14202c' } },
        {
            id: 'rivers',
            type: 'line',
            source: 'rivers',
            paint: { 'line-color': '#2f5f8f', 'line-width': 1 },
        },
        {
            id: 'peaks',
            type: 'circle',
            source: 'peaks',
            filter: ['==', ['get', 'featurecla'], 'mountain'],
            paint: {
                'circle-radius': 3,
                'circle-color': '#8b6f47',
                'circle-stroke-color': '#121418',
                'circle-stroke-width': 1,
            },
        },
        {
            id: 'trail',
            type: 'line',
            source: 'trail',
            paint: {
                'line-color': '#6aa9ff',
                'line-width': 2,
                'line-opacity': 0.8,
                'line-dasharray': [5, 5],
            },
        },
        {
            id: 'heading',
            type: 'line',
            source: 'heading',
            paint: { 'line-color': '#ff7a7a', 'line-width': 3, 'line-opacity': 0.9 },
        },
    ],
};

/* Initialise the map. Synchronous: a GeoJSON source is a URL MapLibre fetches
 * itself, so there is no archive header to read before the map can exist. */
function initMap(): void {
    map = new maplibregl.Map({
        container: 'map',
        style: darkStyle,
        center: DEFAULT_CENTER,
        zoom: DEFAULT_ZOOM,
        maxZoom: 16,
        minZoom: 2,
        attributionControl: false,
    });

    map.addControl(new maplibregl.NavigationControl({ visualizePitch: true }), 'top-right');

    /* Position marker — a prominent dot with a glow. */
    const el = document.createElement('div');
    el.style.width = '20px';
    el.style.height = '20px';
    el.style.borderRadius = '50%';
    el.style.border = '3px solid #6aa9ff';
    el.style.backgroundColor = '#6aa9ff';
    el.style.boxShadow = '0 0 8px rgba(106,169,255,0.8), 0 0 2px rgba(106,169,255,1)';
    marker = new maplibregl.Marker({ element: el }).setLngLat(DEFAULT_CENTER).addTo(map);

    map.on('load', () => {
        trailSource = map!.getSource('trail') as maplibregl.GeoJSONSource;
        headingSource = map!.getSource('heading') as maplibregl.GeoJSONSource;

        /* Named summits: name and true elevation on click. Suppresses the
         * click-to-point for that click only, so inspecting a mountain never
         * commands an antenna by accident. */
        map!.on('click', 'peaks', (e: maplibregl.MapLayerMouseEvent) => {
            const f = e.features?.[0];
            if (!f) {
                return;
            }
            const props = f.properties as { name?: string; elevation?: number };
            const name = props.name ?? 'unnamed';
            const ele = props.elevation !== undefined ? `${Math.round(props.elevation)} m` : 'elevation unknown';
            new maplibregl.Popup({ closeButton: true, offset: 8 })
                .setLngLat(e.lngLat)
                .setHTML(`<b>${name}</b><br>${ele}`)
                .addTo(map!);
        });
        map!.on('mouseenter', 'peaks', () => {
            map!.getCanvas().style.cursor = 'pointer';
        });
        map!.on('mouseleave', 'peaks', () => {
            map!.getCanvas().style.cursor = '';
        });
    });

    /* Click-to-point: report degrees, unconverted. A click that lands on a
     * summit is an inspection, not a command, and is left to the popup. */
    map.on('click', (e: maplibregl.MapMouseEvent) => {
        if (!map || !clickHandler) {
            return;
        }
        const hit = map.queryRenderedFeatures(e.point, { layers: ['peaks'] });
        if (hit.length > 0) {
            return;
        }
        clickHandler(e.lngLat.lat, e.lngLat.lng);
    });

    /* Subscribe to telemetry frames to update position, heading and trail. */
    on('telemetry:frame', (ev: unknown) => {
        const frame = ev as FrameEvent;
        updateFromFrame(frame.view);
    });
}

/* The position the gondola is at, or null. The selected receiver is preferred;
 * if none is selected but one has a valid fix, its fix is used. Absence is
 * null, never a zero coordinate (section 7.1). */
function vehiclePosition(v: View): { lat: number; lon: number; alt: number; ok: boolean } | null {
    let candidate: { lat: number; lon: number; alt: number; ok: boolean } | null = null;
    for (const r of v.gnss) {
        if (r.position === null) {
            continue;
        }
        const pos = { lat: r.position.latitude_deg, lon: r.position.longitude_deg, alt: r.position.altitude_m, ok: r.fix_ok };
        if (r.selected) {
            return pos;
        }
        if (candidate === null) {
            candidate = pos;
        }
    }
    return candidate;
}

/* Compute the endpoint of a heading line given a start point, heading in
 * degrees (0 = north, clockwise), and a length in degrees. Uses the
 * equirectangular approximation, which is accurate enough for a 200 m line. */
function headingEnd(lat: number, lon: number, headingDeg: number, lenDeg: number): [number, number] {
    const rad = (headingDeg * Math.PI) / 180;
    const latRad = (lat * Math.PI) / 180;
    const dLat = lenDeg * Math.cos(rad);
    const dLon = (lenDeg * Math.sin(rad)) / Math.cos(latRad);
    return [lon + dLon, lat + dLat];
}

/* Update marker position, heading line, trail, and readout from a decoded
 * telemetry frame. */
function updateFromFrame(v: View): void {
    const pos = vehiclePosition(v);

    /* Position readout below the map. Absence is stated, never drawn as zero. */
    const posEl = document.querySelector<HTMLElement>('#position');
    if (posEl) {
        posEl.textContent = pos === null
            ? 'Position: no fix'
            : `Lat: ${pos.lat.toFixed(4)}°, Lon: ${pos.lon.toFixed(4)}°, Alt: ${pos.alt.toFixed(1)} m`;
    }

    if (pos === null) {
        /* No fix: park the marker off-view and clear the trail rather than
         * leaving a dot at a stale coordinate pretending to be the gondola. */
        clearTrail();
        if (headingSource) {
            headingSource.setData(EMPTY_LINE);
        }
        return;
    }

    const lat = pos.lat;
    const lon = pos.lon;
    const fixOk = pos.ok;

    if (map && marker) {
        marker.setLngLat([lon, lat]);
        const el = marker.getElement();
        if (el) {
            el.style.backgroundColor = fixOk ? '#6aa9ff' : '#8b909b';
            el.style.borderColor = fixOk ? '#6aa9ff' : '#8b909b';
        }
        /* First fix: bring the gondola into view. Once only, so the operator
         * can pan afterwards without the map fighting them. */
        if (!centeredOnFix) {
            centeredOnFix = true;
            map.jumpTo({ center: [lon, lat], zoom: Math.max(map.getZoom(), FIX_ZOOM) });
        }
    }

    /* Heading line — from the current position in the direction of the IMU
     * heading. Hidden when pico telemetry is absent. */
    if (headingSource) {
        const pico = v.pico;
        if (pico !== null && pico.gondola_heading_deg !== undefined) {
            const end = headingEnd(lat, lon, pico.gondola_heading_deg, HEADING_LEN_DEG);
            headingSource.setData({
                type: 'Feature',
                properties: {},
                geometry: { type: 'LineString', coordinates: [[lon, lat], end] },
            });
        } else {
            headingSource.setData(EMPTY_LINE);
        }
    }

    /* Trail (last TRAIL_MAX points = ~30 s at 1 Hz). */
    trail.push([lon, lat]);
    if (trail.length > TRAIL_MAX) {
        trail.shift();
    }
    if (trailSource && trail.length >= 2) {
        trailSource.setData({
            type: 'Feature',
            properties: {},
            geometry: { type: 'LineString', coordinates: trail },
        });
    }
}

function clearTrail(): void {
    trail.length = 0;
    if (trailSource) {
        trailSource.setData(EMPTY_LINE);
    }
}

export { initMap };
