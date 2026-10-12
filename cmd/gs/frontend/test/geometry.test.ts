/* Unit tests for the pure map geometry.
 *
 * Run with `npm test`, which is `node --test test/*.test.ts`. These hold the
 * arithmetic the map's overlays are built from: the great-circle destination,
 * the antenna pointing sector and the SAR beam footprint. They deliberately do
 * not touch MapLibre or the DOM, which is what lets them run at all -- the same
 * split as widgets.ts and its tests (GUI_ARCHITECTURE.md 9).
 */
import test from 'node:test';
import assert from 'node:assert/strict';

import { destination, sarFootprint, sectorPolygon } from '../src/geometry.ts';

const R = 6371008.8;

/* localOffset returns the east/north metres from a reference point recovered
 * from the great-circle bearing and distance. It is the exact inverse of
 * destination on the tangent plane, so the cone-angle and altitude-scaling
 * invariants below hold to floating-point precision and are not blurred by an
 * equirectangular approximation. */
function localOffset(lat0: number, lon0: number, lat: number, lon: number): [number, number] {
    const p1 = (lat0 * Math.PI) / 180;
    const p2 = (lat * Math.PI) / 180;
    const dl = ((lon - lon0) * Math.PI) / 180;
    const y = Math.sin(dl) * Math.cos(p2);
    const x = Math.cos(p1) * Math.sin(p2) - Math.sin(p1) * Math.cos(p2) * Math.cos(dl);
    const bearing = Math.atan2(y, x);
    const c = Math.sin(p1) * Math.sin(p2) + Math.cos(p1) * Math.cos(p2) * Math.cos(dl);
    const dist = Math.acos(Math.min(1, Math.max(-1, c))) * R;
    return [dist * Math.sin(bearing), dist * Math.cos(bearing)];
}

test('destination walks a known arc north and east', () => {
    const oneDeg = (R * Math.PI) / 180;
    const [lonN, latN] = destination(0, 0, 0, oneDeg);
    assert.ok(Math.abs(latN - 1) < 1e-9, `north lat ${latN}`);
    assert.ok(Math.abs(lonN) < 1e-9, `north lon ${lonN}`);

    const [lonE, latE] = destination(0, 0, 90, oneDeg);
    assert.ok(Math.abs(lonE - 1) < 1e-9, `east lon ${lonE}`);
    assert.ok(Math.abs(latE) < 1e-9, `east lat ${latE}`);
});

test('sectorPolygon refuses a degenerate wedge', () => {
    assert.equal(sectorPolygon(61, 8, 90, 0, 1000, 16), null);
    assert.equal(sectorPolygon(61, 8, 90, -5, 1000, 16), null);
    assert.equal(sectorPolygon(61, 8, 90, 36, 0, 16), null);
    assert.equal(sectorPolygon(61, 8, Number.NaN, 36, 1000, 16), null);
    assert.equal(sectorPolygon(Number.NaN, 8, 90, 36, 1000, 16), null);
});

test('sectorPolygon spans the requested bearings at the requested radius', () => {
    const lat = 61.5;
    const lon = 8.5;
    const f = sectorPolygon(lat, lon, 90, 36, 1000, 24);
    assert.ok(f !== null);
    const ring = f!.geometry.coordinates[0];
    // centre, 25 arc points, centre again
    assert.equal(ring.length, 27);
    assert.deepEqual(ring[0], [lon, lat]);
    assert.deepEqual(ring[ring.length - 1], [lon, lat]);

    for (let i = 1; i < ring.length - 1; i++) {
        const [e, n] = localOffset(lat, lon, ring[i][1], ring[i][0]);
        const dist = Math.hypot(e, n);
        assert.ok(Math.abs(dist - 1000) < 1, `arc radius ${dist}`);
        const bearing = ((Math.atan2(e, n) * 180) / Math.PI + 360) % 360;
        // Bearing 90 ±36, accounting for the wrap at 0/360.
        let delta = bearing - 90;
        if (delta > 180) delta -= 360;
        if (delta < -180) delta += 360;
        assert.ok(Math.abs(delta) <= 36 + 1e-6, `arc bearing ${bearing}`);
    }
});

test('sarFootprint refuses an aircraft on the ground or a bad geometry', () => {
    assert.equal(sarFootprint(61, 8, 90, 0, 45, 10, 64), null);
    assert.equal(sarFootprint(61, 8, 90, -100, 45, 10, 64), null);
    assert.equal(sarFootprint(61, 8, Number.NaN, 1000, 45, 10, 64), null);
    assert.equal(sarFootprint(61, 8, 90, 1000, 90, 10, 64), null);
    assert.equal(sarFootprint(61, 8, 90, 1000, 0, 10, 64), null);
    assert.equal(sarFootprint(61, 8, 90, 1000, 45, 0, 64), null);
});

test('every SAR footprint vertex sits on the beam cone', () => {
    const lat = 61.5;
    const lon = 8.5;
    const bearing = 137;
    const alt = 3000;
    const down = 45;
    const fov = 10;
    const f = sarFootprint(lat, lon, bearing, alt, down, fov, 64);
    assert.ok(f !== null);
    const ring = f!.geometry.coordinates[0];
    assert.deepEqual(ring[0], ring[ring.length - 1], 'ring is closed');

    const br = (bearing * Math.PI) / 180;
    const dep = (down * Math.PI) / 180;
    const ax = Math.sin(br) * Math.cos(dep);
    const ay = Math.cos(br) * Math.cos(dep);
    const az = -Math.sin(dep);
    const target = Math.cos((fov / 2) * (Math.PI / 180));

    for (let i = 0; i < ring.length - 1; i++) {
        const [e, n] = localOffset(lat, lon, ring[i][1], ring[i][0]);
        // Vector from the aperture at (0, 0, alt) to the ground point.
        const vx = e;
        const vy = n;
        const vz = -alt;
        const len = Math.hypot(vx, vy, vz);
        const cosAngle = (vx * ax + vy * ay + vz * az) / len;
        assert.ok(Math.abs(cosAngle - target) < 1e-6, `cone angle at vertex ${i}: ${cosAngle}`);
    }
});

test('the SAR footprint scales with altitude', () => {
    const lat = 61.5;
    const lon = 8.5;
    const a = sarFootprint(lat, lon, 0, 1000, 45, 10, 32)!;
    const b = sarFootprint(lat, lon, 0, 2000, 45, 10, 32)!;
    const ra = a.geometry.coordinates[0];
    const rb = b.geometry.coordinates[0];
    for (let i = 0; i < ra.length; i++) {
        const [ea, na] = localOffset(lat, lon, ra[i][1], ra[i][0]);
        const [eb, nb] = localOffset(lat, lon, rb[i][1], rb[i][0]);
        assert.ok(Math.abs(eb - 2 * ea) < 0.01, `east scales at ${i}`);
        assert.ok(Math.abs(nb - 2 * na) < 0.01, `north scales at ${i}`);
    }
});

test('the SAR footprint lies along the commanded bearing', () => {
    const lat = 61.5;
    const lon = 8.5;
    const f = sarFootprint(lat, lon, 90, 2000, 45, 10, 64)!;
    const ring = f.geometry.coordinates[0];
    let sumE = 0;
    let sumN = 0;
    for (let i = 0; i < ring.length - 1; i++) {
        const [e, n] = localOffset(lat, lon, ring[i][1], ring[i][0]);
        sumE += e;
        sumN += n;
    }
    const meanE = sumE / (ring.length - 1);
    const meanN = sumN / (ring.length - 1);
    // Bearing 90 is due east, so the patch centre is east and roughly on the
    // east-west axis. At 45 degrees depression the axis reaches the ground at
    // the altitude itself; the mean of the boundary is within a footprint
    // width of it.
    assert.ok(meanE > 0, `mean east ${meanE}`);
    assert.ok(Math.abs(meanN) < 2000, `mean north ${meanN}`);
});
