/* ROCSAR Ground Station — Offline Map.
 *
 * Displays GNSS position over an offline basemap built from two committed
 * assets, both rendered by MapLibre GL JS and served locally from the Go binary
 * (pre-gzipped, see cmd/gs/bridge.go) — zero network at runtime:
 *
 *   - Natural Earth GeoJSON: global ocean, land, mountain ranges and named
 *     summits at 50m, plus lakes and rivers at 10m clipped to the operating
 *     area — the 50m hydrography has only 58 lakes and 34 rivers across all of
 *     Northern Europe (scripts/fetch-basemap.py).
 *   - Natural Earth shaded relief: a single cropped, Web-Mercator-projected
 *     image over the operating area, giving the terrain its shape
 *     (scripts/fetch-relief/main.go).
 *
 * Why GeoJSON and a baked image rather than tiles: GUI_ARCHITECTURE.md 1.2. The
 * map needs regional context, not a street basemap, and Natural Earth is public
 * domain and generalised for exactly this scale. A DEM-tile pyramid fine enough
 * to beat the relief image costs tens of megabytes; the image costs a fraction
 * of one. The relief is reprojected at build time because MapLibre places an
 * image source by mapping its corners linearly, which cannot represent the
 * non-linear latitude of Web Mercator — see the tool's package comment.
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

/* Bearing lines are drawn at a fixed PIXEL length, not a fixed geographic one.
 * The previous fixed 0.002° was sub-pixel at the default zoom and only ~6 px at
 * zoom 12, which is why the heading line never appeared. The geographic length
 * is derived from the current zoom on every redraw. Web Mercator is conformal —
 * its scale is the same in every direction at a point — so a line of N pixels
 * comes out N pixels long whichever way it points, and no screen-space maths
 * (and so no map-rotation handling) is needed. */
const HEADING_PX = 46; // gondola heading (IMU): which way the platform faces
const TARGET_PX = 78; // antenna target bearing: where the antennas point

/* Ground scale at the equator and zoom 0, in metres per pixel. This is the
 * 512-px-tile value (equatorial circumference / 512), because MapLibre's
 * transform uses tileSize = 512 (geo/transform.ts) and worldSize = 512 · 2^zoom.
 * The common 156543.03392 figure is the 256-px-tile convention and would draw
 * these lines twice as long. */
const EQUATOR_M_PER_PX_Z0 = 78271.51696;
const EARTH_RADIUS_M = 6371008.8;

/* Empty data for hiding a source. A FeatureCollection, not a zeroed feature:
 * an empty LineString or a Point at [0,0] would otherwise render at Null
 * Island. */
const EMPTY_FC: GeoJSON.FeatureCollection = { type: 'FeatureCollection', features: [] };

let map: maplibregl.Map | null = null;
let marker: maplibregl.Marker | null = null;
let trailSource: maplibregl.GeoJSONSource | null = null;
let headingSource: maplibregl.GeoJSONSource | null = null;
let targetSource: maplibregl.GeoJSONSource | null = null;
let headingTipSource: maplibregl.GeoJSONSource | null = null;
let targetTipSource: maplibregl.GeoJSONSource | null = null;
const trail: [number, number][] = [];
let centeredOnFix = false;

/* Latest fix and bearings, kept so the bearing lines can be redrawn when the
 * camera settles. Their length is zoom-dependent, so a zoom would otherwise
 * leave them the wrong geographic length until the next 1 Hz frame. */
let lastFix: [number, number] | null = null;
let gondolaHeadingDeg: number | null = null;
let targetHeadingDeg: number | null = null;
let gondolaHeld = false;

/* Click-to-point callback. Set by main.ts when the command path is ready.
 * Receives latitude and longitude in degrees, unconverted (section 7.3). */
let clickHandler: ((lat: number, lon: number) => void) | null = null;

export function setMapClickHandler(fn: ((lat: number, lon: number) => void) | null): void {
    clickHandler = fn;
}

/* The dark basemap. Palette matches the console (frontend/src/style.css):
 * water is the one saturated hue, land and relief stay near the page
 * background, and the gondola marker is the brightest thing on the map so the
 * eye finds it without a legend.
 *
 * Layer order is load-bearing. The relief image covers its whole bounding box
 * (land and sea alike), so:
 *   land   -- an opaque base under the relief, for terrain OUTSIDE the AOI,
 *             where there is no relief image and the ground would otherwise be
 *             page-background
 *   relief -- the shaded relief, inside the AOI only
 *   ocean  -- opaque, drawn OVER the relief so the sea reads as water rather
 *             than as the relief's flat white
 * Anything reordered below those three changes which of them wins. */
const darkStyle: maplibregl.StyleSpecification = {
    version: 8,
    sources: {
        /* Natural Earth 50m/10m shaded relief, cropped and warped to Web
         * Mercator by scripts/fetch-relief. The corners are clockwise from the
         * top-left; the image is already Mercator, so the quad through these
         * four points is exact. */
        relief: {
            type: 'image',
            url: '/relief/ne_relief.jpg',
            coordinates: [
                [-12, 72],
                [45, 72],
                [45, 50],
                [-12, 50],
            ],
        },
        ocean: { type: 'geojson', data: `${GEOJSON}/ne_50m_ocean.json` },
        land: { type: 'geojson', data: `${GEOJSON}/ne_50m_land.json` },
        lakes: { type: 'geojson', data: `${GEOJSON}/ne_lakes.json` },
        rivers: { type: 'geojson', data: `${GEOJSON}/ne_rivers.json` },
        ranges: { type: 'geojson', data: `${GEOJSON}/ne_50m_mountain_ranges.json` },
        peaks: { type: 'geojson', data: `${GEOJSON}/ne_50m_geography_regions_elevation_points.json` },
        trail: { type: 'geojson', data: EMPTY_FC },
        heading: { type: 'geojson', data: EMPTY_FC },
        target: { type: 'geojson', data: EMPTY_FC },
        headingTip: { type: 'geojson', data: EMPTY_FC },
        targetTip: { type: 'geojson', data: EMPTY_FC },
    },
    layers: [
        { id: 'background', type: 'background', paint: { 'background-color': '#121418' } },
        { id: 'land', type: 'fill', source: 'land', paint: { 'fill-color': '#1d222b' } },
        {
            id: 'relief',
            type: 'raster',
            source: 'relief',
            paint: { 'raster-opacity': 1, 'raster-resampling': 'linear' },
        },
        { id: 'ocean', type: 'fill', source: 'ocean', paint: { 'fill-color': '#14202c' } },
        /* Coastline, drawn above the relief: the land fill's own outline would
         * sit under the raster (land -> relief -> ocean), so the shore needs a
         * layer of its own once the raster is in the stack. It reuses the land
         * source, so it costs no new data. */
        {
            id: 'coastline',
            type: 'line',
            source: 'land',
            paint: { 'line-color': 'rgba(104,158,190,0.5)', 'line-width': 0.7 },
        },
        {
            id: 'ranges',
            type: 'fill',
            source: 'ranges',
            paint: { 'fill-color': '#2b2118', 'fill-opacity': 0.18 },
        },
        {
            id: 'ranges-outline',
            type: 'line',
            source: 'ranges',
            paint: { 'line-color': '#4a3a28', 'line-width': 0.8, 'line-opacity': 0.7 },
        },
        {
            id: 'lakes',
            type: 'fill',
            source: 'lakes',
            paint: {
                'fill-color': '#1d4a63',
                'fill-outline-color': 'rgba(94,158,198,0.7)',
            },
        },
        {
            id: 'rivers',
            type: 'line',
            source: 'rivers',
            paint: { 'line-color': '#3d7fae', 'line-width': 1.1, 'line-opacity': 0.9 },
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
            paint: { 'line-color': '#22d3ee', 'line-width': 3, 'line-opacity': 0.95 },
        },
        {
            id: 'target',
            type: 'line',
            source: 'target',
            paint: { 'line-color': '#ffb454', 'line-width': 3, 'line-opacity': 0.95 },
        },
        {
            id: 'headingTip',
            type: 'symbol',
            source: 'headingTip',
            layout: {
                'icon-image': 'bearing-arrow-heading',
                'icon-rotate': ['get', 'bearing'],
                'icon-rotation-alignment': 'map',
                'icon-size': 0.75,
                'icon-allow-overlap': true,
                'icon-ignore-placement': true,
            },
        },
        {
            id: 'targetTip',
            type: 'symbol',
            source: 'targetTip',
            layout: {
                'icon-image': 'bearing-arrow-target',
                'icon-rotate': ['get', 'bearing'],
                'icon-rotation-alignment': 'map',
                'icon-size': 0.75,
                'icon-allow-overlap': true,
                'icon-ignore-placement': true,
            },
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
    /* Ground scale for bearing lines and for judging distance to a clicked
     * point. Metric, because every other number on this console is. */
    map.addControl(new maplibregl.ScaleControl({ maxWidth: 140, unit: 'metric' }), 'bottom-left');

    /* Position marker — deliberately small. The bearing lines carry the
     * orientation and the trail carries the history; a large dot only hides the
     * ground it marks. box-sizing keeps the 2 px outline inside the 11 px, so
     * the rendered size is 11 px rather than 15 px. */
    const el = document.createElement('div');
    el.style.boxSizing = 'border-box';
    el.style.width = '11px';
    el.style.height = '11px';
    el.style.borderRadius = '50%';
    el.style.border = '2px solid #0d1014';
    el.style.backgroundColor = '#6aa9ff';
    el.style.boxShadow = '0 0 4px rgba(106,169,255,0.9)';
    marker = new maplibregl.Marker({ element: el }).setLngLat(DEFAULT_CENTER).addTo(map);

    map.on('load', () => {
        trailSource = map!.getSource('trail') as maplibregl.GeoJSONSource;
        headingSource = map!.getSource('heading') as maplibregl.GeoJSONSource;
        targetSource = map!.getSource('target') as maplibregl.GeoJSONSource;
        headingTipSource = map!.getSource('headingTip') as maplibregl.GeoJSONSource;
        targetTipSource = map!.getSource('targetTip') as maplibregl.GeoJSONSource;
        ensureArrowImages();
        redrawBearings();

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

    /* Bearing arrowheads. MapLibre has no built-in arrow and the console is
     * offline, so the two icons are drawn on a canvas. They are registered
     * through styleimagemissing, which is the supported way to satisfy a style
     * that references an icon before it exists. */
    map.on('styleimagemissing', (e: maplibregl.MapStyleImageMissingEvent) => {
        if (e.id === 'bearing-arrow-heading' || e.id === 'bearing-arrow-target') {
            ensureArrowImages();
        }
    });

    /* A camera settle (zoom, pan or rotate) changes the metres per pixel, so
     * the bearing lines need their geographic length recomputed. */
    map.on('moveend', () => redrawBearings());

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

/* arrowImage draws a north-pointing arrowhead for a symbol layer. Rotated by
 * icon-rotate = the feature's bearing, it marks the end of a bearing line. */
function arrowImage(color: string): ImageData {
    const s = 24;
    const c = document.createElement('canvas');
    c.width = s;
    c.height = s;
    const ctx = c.getContext('2d');
    if (ctx === null) {
        return new ImageData(s, s);
    }
    ctx.fillStyle = color;
    ctx.beginPath();
    ctx.moveTo(s / 2, 1);
    ctx.lineTo(s - 1, s - 1);
    ctx.lineTo(s / 2, s * 0.62);
    ctx.lineTo(1, s - 1);
    ctx.closePath();
    ctx.fill();
    return ctx.getImageData(0, 0, s, s);
}

/* ensureArrowImages registers the two arrowhead icons. Idempotent, and called
 * both on load and from styleimagemissing: a symbol layer that references an
 * icon the style does not yet have is the one way these arrows can silently
 * fail to appear, so both paths are covered. */
function ensureArrowImages(): void {
    if (map === null) {
        return;
    }
    if (!map.hasImage('bearing-arrow-heading')) {
        map.addImage('bearing-arrow-heading', arrowImage('#22d3ee'));
    }
    if (!map.hasImage('bearing-arrow-target')) {
        map.addImage('bearing-arrow-target', arrowImage('#ffb454'));
    }
}

/* destination returns the point reached by travelling distM metres from
 * (lat, lon) on the given bearing (degrees, 0 = north, clockwise). The
 * great-circle formula, so the bearing stays true away from the equator. */
function destination(lat: number, lon: number, bearingDeg: number, distM: number): [number, number] {
    const d = distM / EARTH_RADIUS_M;
    const th = (bearingDeg * Math.PI) / 180;
    const p1 = (lat * Math.PI) / 180;
    const l1 = (lon * Math.PI) / 180;
    const p2 = Math.asin(Math.sin(p1) * Math.cos(d) + Math.cos(p1) * Math.sin(d) * Math.cos(th));
    const l2 = l1 + Math.atan2(Math.sin(th) * Math.sin(d) * Math.cos(p1), Math.cos(d) - Math.sin(p1) * Math.sin(p2));
    return [(l2 * 180) / Math.PI, (p2 * 180) / Math.PI];
}

/* bearingTo returns the initial great-circle bearing (degrees, 0 = north,
 * clockwise) from the current fix to a target. This is what click-to-point
 * commands: an antenna command is a bearing, and a click gives a position, so
 * the two are not interchangeable — the stub this replaced sent the target's
 * latitude as if it were a bearing. Returns null with no fix, so a click before
 * the first fix commands nothing rather than a bearing from nowhere. */
export function bearingTo(lat: number, lon: number): number | null {
    if (lastFix === null) {
        return null;
    }
    const [fromLon, fromLat] = lastFix;
    const p1 = (fromLat * Math.PI) / 180;
    const p2 = (lat * Math.PI) / 180;
    const dl = ((lon - fromLon) * Math.PI) / 180;
    const y = Math.sin(dl) * Math.cos(p2);
    const x = Math.cos(p1) * Math.sin(p2) - Math.sin(p1) * Math.cos(p2) * Math.cos(dl);
    return ((Math.atan2(y, x) * 180) / Math.PI + 360) % 360;
}

/* metresPerPixel is the ground scale at a latitude for the current zoom. Web
 * Mercator is conformal, so it is the scale in every direction and a fixed
 * pixel length comes out the same apparent length at any bearing. */function metresPerPixel(lat: number): number {
    if (map === null) {
        return 0;
    }
    return (EQUATOR_M_PER_PX_Z0 * Math.cos((lat * Math.PI) / 180)) / Math.pow(2, map.getZoom());
}

/* bearingLine builds the line and the arrowhead tip for one bearing. */
function bearingLine(
    from: [number, number],
    bearingDeg: number,
    px: number,
): { line: GeoJSON.Feature<GeoJSON.LineString>; tip: GeoJSON.Feature<GeoJSON.Point> } {
    const [lon, lat] = from;
    const end = destination(lat, lon, bearingDeg, px * metresPerPixel(lat));
    return {
        line: { type: 'Feature', properties: {}, geometry: { type: 'LineString', coordinates: [from, end] } },
        tip: { type: 'Feature', properties: { bearing: bearingDeg }, geometry: { type: 'Point', coordinates: end } },
    };
}

/* redrawBearings redraws both bearing lines and their arrowheads. Called on
 * every telemetry frame and whenever the camera settles. Absence is absent: a
 * missing fix or missing flight-controller telemetry clears a line rather than
 * drawing one at a default bearing. */
function redrawBearings(): void {
    const hide = (line: maplibregl.GeoJSONSource | null, tip: maplibregl.GeoJSONSource | null): void => {
        line?.setData(EMPTY_FC);
        tip?.setData(EMPTY_FC);
    };

    if (lastFix === null) {
        hide(headingSource, headingTipSource);
        hide(targetSource, targetTipSource);
        return;
    }
    if (gondolaHeadingDeg !== null) {
        const b = bearingLine(lastFix, gondolaHeadingDeg, HEADING_PX);
        headingSource?.setData(b.line);
        headingTipSource?.setData(b.tip);
    } else {
        hide(headingSource, headingTipSource);
    }
    if (targetHeadingDeg !== null) {
        const b = bearingLine(lastFix, targetHeadingDeg, TARGET_PX);
        targetSource?.setData(b.line);
        targetTipSource?.setData(b.tip);
    } else {
        hide(targetSource, targetTipSource);
    }
}

/* bearingReadout names the two bearing lines in the colours they are drawn in,
 * so the readout doubles as the legend. A held heading is called out, because
 * it is the last value the flight controller had rather than a measurement
 * (section 7.2). */
function bearingReadout(): string {
    const deg = (d: number | null): string => (d === null ? '&mdash;' : `${d.toFixed(1)}&deg;`);
    const held = gondolaHeld ? ' <i>(held)</i>' : '';
    return ` &nbsp; <span style="color:#22d3ee">&#9679;</span> heading ${deg(gondolaHeadingDeg)}${held}` +
        ` &nbsp; <span style="color:#ffb454">&#9679;</span> target ${deg(targetHeadingDeg)}`;
}

/* Update marker position, bearing lines, trail, and readout from a decoded
 * telemetry frame. */
function updateFromFrame(v: View): void {
    const pos = vehiclePosition(v);

    const pico = v.pico ?? null;
    gondolaHeadingDeg = pico === null ? null : pico.gondola_heading_deg;
    targetHeadingDeg = pico === null ? null : pico.target_heading_deg;
    gondolaHeld = pico !== null && pico.imu === 'HELD';

    /* Position readout below the map. Absence is stated, never drawn as zero. */
    const posEl = document.querySelector<HTMLElement>('#position');
    if (posEl) {
        posEl.innerHTML = pos === null
            ? 'Position: no fix'
            : `Lat: ${pos.lat.toFixed(4)}&deg;, Lon: ${pos.lon.toFixed(4)}&deg;, Alt: ${pos.alt.toFixed(1)} m` +
              bearingReadout();
    }

    if (pos === null) {
        /* No fix: clear the trail and the bearings rather than leaving a dot and
         * two lines at a stale coordinate pretending to be the gondola. */
        lastFix = null;
        clearTrail();
        redrawBearings();
        return;
    }

    lastFix = [pos.lon, pos.lat];
    const fixOk = pos.ok;

    if (map && marker) {
        marker.setLngLat([pos.lon, pos.lat]);
        const el = marker.getElement();
        if (el) {
            el.style.backgroundColor = fixOk ? '#6aa9ff' : '#8b909b';
        }
        /* First fix: bring the gondola into view. Once only, so the operator
         * can pan afterwards without the map fighting them. */
        if (!centeredOnFix) {
            centeredOnFix = true;
            map.jumpTo({ center: [pos.lon, pos.lat], zoom: Math.max(map.getZoom(), FIX_ZOOM) });
        }
    }

    redrawBearings();

    /* Trail (last TRAIL_MAX points = ~30 s at 1 Hz). */
    trail.push([pos.lon, pos.lat]);
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
        trailSource.setData(EMPTY_FC);
    }
}

export { initMap };
