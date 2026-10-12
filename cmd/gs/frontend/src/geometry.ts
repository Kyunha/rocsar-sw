/* ROCSAR Ground Station — pure map geometry.
 *
 * The arithmetic behind the map's dynamic overlays: the antenna pointing
 * sector and the SAR beam footprint. Nothing here touches MapLibre, the DOM or
 * telemetry state — these are plain functions over plain numbers that return
 * GeoJSON features. That is what lets test/geometry.test.ts hold the maths
 * without a browser, and it is the same reason the instruments live in
 * widgets.ts (GUI_ARCHITECTURE.md 7.6, 9).
 *
 * Coordinates in and out are degrees; distances are metres. A GeoJSON position
 * is [longitude, latitude], and every function here returns that order.
 */

/* WGS-84 mean radius, in metres. */
const EARTH_RADIUS_M = 6371008.8;

/* destination returns the point reached by travelling distM metres from
 * (lat, lon) on the given bearing (degrees, 0 = north, clockwise), as
 * [lon, lat]. The great-circle formula, so a bearing stays true away from the
 * equator — the map draws at 50-72 N, where a flat-earth shortcut is visibly
 * wrong over more than a few kilometres. */
export function destination(lat: number, lon: number, bearingDeg: number, distM: number): [number, number] {
    const d = distM / EARTH_RADIUS_M;
    const th = (bearingDeg * Math.PI) / 180;
    const p1 = (lat * Math.PI) / 180;
    const l1 = (lon * Math.PI) / 180;
    const p2 = Math.asin(Math.sin(p1) * Math.cos(d) + Math.cos(p1) * Math.sin(d) * Math.cos(th));
    const l2 = l1 + Math.atan2(Math.sin(th) * Math.sin(d) * Math.cos(p1), Math.cos(d) - Math.sin(p1) * Math.sin(p2));
    return [(l2 * 180) / Math.PI, (p2 * 180) / Math.PI];
}

/* sectorPolygon builds the wedge of bearings an antenna axis can reach:
 * centerBearingDeg ± halfAngleDeg, at a fixed radius. It is the shape of the
 * servo's mechanical limit projected on the map, not the beam.
 *
 * Returns null for a non-finite position or a non-positive half-angle or
 * radius, so a caller clears the layer rather than drawing a degenerate wedge.
 * samples is the number of arc segments; the ring is closed. */
export function sectorPolygon(
    lat: number,
    lon: number,
    centerBearingDeg: number,
    halfAngleDeg: number,
    radiusM: number,
    samples: number,
): GeoJSON.Feature<GeoJSON.Polygon> | null {
    if (!isFinitePoint(lat, lon) || !Number.isFinite(centerBearingDeg)
        || !(halfAngleDeg > 0) || !(radiusM > 0)) {
        return null;
    }
    const ring: [number, number][] = [[lon, lat]];
    const n = Math.max(2, Math.floor(samples));
    for (let i = 0; i <= n; i++) {
        const b = centerBearingDeg - halfAngleDeg + (2 * halfAngleDeg * i) / n;
        ring.push(destination(lat, lon, b, radiusM));
    }
    ring.push([lon, lat]);
    return { type: 'Feature', properties: {}, geometry: { type: 'Polygon', coordinates: [ring] } };
}

/* sarFootprint returns the patch of ground the SAR beam lights up.
 *
 * A cone of half-angle fovDeg / 2 whose axis leaves the aircraft at bearingDeg
 * and downAngleDeg below horizontal, meeting the ground plane altitudeM below.
 * The intersection of a cone with a plane is a conic — here an ellipse — and
 * rather than special-case the closed form the boundary is traced by
 * intersecting a fan of rays around the axis with the plane. That is exact for
 * flat ground, and it stays correct if the angles are ever changed to values
 * whose closed form has a different branch.
 *
 * The plane is sea level: the console ships no terrain model (GUI_ARCHITECTURE
 * .md 1.2.1 records why), so over high ground this is the footprint on the
 * datum, not on the hillside — an operator reads it against the relief and sees
 * where the ground rises to meet the beam.
 *
 * Returns null without a positive altitude or with a geometry that never
 * reaches the ground — an aircraft on the ground, a missing fix or a beam
 * pointed above the horizon has no footprint to draw. */
export function sarFootprint(
    lat: number,
    lon: number,
    bearingDeg: number,
    altitudeM: number,
    downAngleDeg: number,
    fovDeg: number,
    samples: number,
): GeoJSON.Feature<GeoJSON.Polygon> | null {
    if (!isFinitePoint(lat, lon) || !Number.isFinite(bearingDeg)
        || !(altitudeM > 0) || !(fovDeg > 0) || !(downAngleDeg > 0 && downAngleDeg < 90)) {
        return null;
    }
    const br = (bearingDeg * Math.PI) / 180;
    const dep = (downAngleDeg * Math.PI) / 180;
    const half = (fovDeg / 2) * (Math.PI / 180);

    /* Local ENU frame at the nadir: x east, y north, z up. The beam axis, and
     * an orthonormal pair (u, v) spanning the plane perpendicular to it. u is
     * horizontal, so rotating the axis towards u turns the beam in azimuth and
     * towards v changes its elevation. */
    const ax = Math.sin(br) * Math.cos(dep);
    const ay = Math.cos(br) * Math.cos(dep);
    const az = -Math.sin(dep);
    const ux = Math.cos(br);
    const uy = -Math.sin(br);
    const vx = -Math.sin(dep) * Math.sin(br);
    const vy = -Math.sin(dep) * Math.cos(br);
    const vz = -Math.cos(dep);

    const ca = Math.cos(half);
    const sa = Math.sin(half);

    const ring: [number, number][] = [];
    const n = Math.max(8, Math.floor(samples));
    for (let i = 0; i < n; i++) {
        const phi = (2 * Math.PI * i) / n;
        const cphi = Math.cos(phi);
        const sphi = Math.sin(phi);
        const rx = ax * ca + (ux * cphi + vx * sphi) * sa;
        const ry = ay * ca + (uy * cphi + vy * sphi) * sa;
        const rz = az * ca + vz * sphi * sa;
        if (!(rz < 0)) {
            return null;
        }
        const t = -altitudeM / rz;
        const x = t * rx;
        const y = t * ry;
        const dist = Math.hypot(x, y);
        const groundBearing = (Math.atan2(x, y) * 180) / Math.PI;
        ring.push(destination(lat, lon, (groundBearing + 360) % 360, dist));
    }
    ring.push(ring[0]);
    return { type: 'Feature', properties: {}, geometry: { type: 'Polygon', coordinates: [ring] } };
}

/* isFinitePoint guards a coordinate pair before it reaches the great-circle
 * maths, where a NaN would propagate into a whole polygon of NaN positions. */
function isFinitePoint(lat: number, lon: number): boolean {
    return Number.isFinite(lat) && Number.isFinite(lon) && lat >= -90 && lat <= 90;
}
