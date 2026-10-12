/* Unit tests for the instrument widgets.
 *
 * Run with `npm test`, which is `node --test test/`. Node 24 strips the types
 * itself, so this needs no framework, no transpile step and no dependency --
 * which is the only kind of test this frontend could have without contradicting
 * the no-framework rule in GUI_ARCHITECTURE.md 9.
 *
 * These cover the arithmetic and the two absence rules (7.1, 7.2) that a future
 * widget is most likely to get wrong. They do not cover how anything looks;
 * that is checked by rendering the SVG, which these cannot do.
 */
import test from 'node:test';
import assert from 'node:assert/strict';

import {
    Ring,
    barGauge,
    budgetBar,
    clamp,
    clockText,
    compass,
    durationText,
    knobValueFromPointer,
    percent,
    shortestDelta,
    sparkline,
    stateChip,
    tickAngle,
    TICK_MAX,
    renderShotResult,
} from '../src/widgets.ts';

test('shortestDelta takes the short way round', () => {
    assert.equal(shortestDelta(10, 20), 10);
    assert.equal(shortestDelta(350, 10), 20);
    assert.equal(shortestDelta(10, 350), -20);
    assert.equal(shortestDelta(0, 0), 0);
});

test('shortestDelta reports the antipode as +180, not -180', () => {
    assert.equal(shortestDelta(0, 180), 180);
    assert.equal(shortestDelta(180, 0), 180);
    assert.equal(shortestDelta(90, 270), 180);
});

test('clamp and percent refuse to produce NaN or exceed the range', () => {
    assert.equal(clamp(5, 0, 10), 5);
    assert.equal(clamp(-1, 0, 10), 0);
    assert.equal(clamp(11, 0, 10), 10);
    assert.equal(percent(50, 100), 50);
    assert.equal(percent(150, 100), 100);
    assert.equal(percent(-5, 100), 0);
    assert.equal(percent(1, 0), 0);
    assert.equal(percent(Number.NaN, 100), 0);
});

test('tickAngle spans the servo range and saturates at the ends', () => {
    assert.equal(tickAngle(0), 0);
    assert.equal(tickAngle(TICK_MAX), 360);
    assert.ok(Math.abs(tickAngle(2047.5) - 180) < 0.02);
    assert.equal(tickAngle(-100), 0);
    assert.equal(tickAngle(99999), 360);
});

test('durationText and clockText format spans and instants', () => {
    assert.equal(durationText(0), '0:00');
    assert.equal(durationText(59), '0:59');
    assert.equal(durationText(60), '1:00');
    assert.equal(durationText(3600), '1:00:00');
    assert.equal(durationText(3721), '1:02:01');
    assert.equal(durationText(-1), '-');
    assert.equal(clockText(-1), '-');
    assert.equal(clockText(3600, new Date('2020-01-01T10:00:00')), '11:00');
});

test('Ring is bounded and ignores non-finite samples', () => {
    const r = new Ring(3);
    for (const v of [1, 2, 3, 4, 5]) {
        r.push(v);
    }
    assert.deepEqual(r.values(), [3, 4, 5]);
    assert.equal(r.length, 3);
    r.push(Number.NaN);
    assert.deepEqual(r.values(), [3, 4, 5]);
});

test('compass names both headings and the error', () => {
    const svg = compass({ currentDeg: 10, targetDeg: 20 });
    assert.match(svg, /heading 10\.0 degrees, target 20\.0 degrees, error \+10\.0/);
    assert.match(svg, /w-arc/);
    assert.match(svg, /target 20\.0°/);
});

test('compass says "on target" inside the deadband and draws no arc', () => {
    const svg = compass({ currentDeg: 10, targetDeg: 10.2 });
    assert.match(svg, /on target/);
    assert.doesNotMatch(svg, /w-arc/);
});

test('compass still draws an arc for an antipodal error', () => {
    const svg = compass({ currentDeg: 0, targetDeg: 180 });
    assert.match(svg, /w-arc/);
    assert.match(svg, /\+180\.0/);
});

test('compass marks a held heading as held', () => {
    const svg = compass({ currentDeg: 10, targetDeg: 20, held: true });
    assert.match(svg, /\(held\)/);
    assert.match(svg, /w-needle-current w-held/);
});

test('barGauge renders a held reading as a word, never a number', () => {
    const html = barGauge({ label: 'load', value: null, max: 100, unit: '%' });
    assert.match(html, /w-bar-held/);
    assert.match(html, /held/);
    assert.doesNotMatch(html, /w-bar-fill/);
    // The word "held" is the only content; there must be no numeric value.
    assert.doesNotMatch(html, />\s*0\s*</);
});

test('barGauge renders a measurement as a sized bar', () => {
    const html = barGauge({ label: 'load', value: 42, max: 100, unit: '%' });
    assert.match(html, /w-bar-fill/);
    assert.match(html, /width:42\.0%/);
    assert.match(html, />42%</);
});

test('barGauge colours an over-threshold reading by class, not by value alone', () => {
    const html = barGauge({ label: 'temp', value: 80, max: 100, unit: 'C', badAt: 75 });
    assert.match(html, /w-bad/);
    assert.match(html, />80C</);
});

test('budgetBar draws measured throughput against the cap', () => {
    const html = budgetBar({ capKbps: 115, measuredKbps: 46, enforced: true });
    assert.match(html, /cap 115 kbit\/s/);
    assert.match(html, /width:40\.0%/);
    assert.match(html, /measured <b>46<\/b> kbit\/s/);
    assert.doesNotMatch(html, /w-bad/);
    assert.doesNotMatch(html, /not enforced/);
});

test('budgetBar reports no reading rather than an idle link', () => {
    const html = budgetBar({ capKbps: 115, measuredKbps: null, enforced: true });
    assert.match(html, /no reading/);
    assert.match(html, /w-bar-held/);
    assert.doesNotMatch(html, /w-bar-fill/);
});

test('budgetBar marks an over-cap reading instead of clamping it silently', () => {
    // HTB is a scheduler, not a policer: measured can exceed the cap. The bar
    // fills and warns, and the number stays truthful.
    const html = budgetBar({ capKbps: 115, measuredKbps: 200, enforced: true });
    assert.match(html, /w-bad/);
    assert.match(html, /width:100\.0%/);
    assert.match(html, /measured <b>200<\/b> kbit\/s — over cap/);
});

test('budgetBar says when the cap is not being enforced', () => {
    const html = budgetBar({ capKbps: 115, measuredKbps: 46, enforced: false });
    assert.match(html, /cap not enforced/);
});

test('sparkline states the absence of history instead of drawing a flat line', () => {
    assert.match(sparkline({ values: [], label: 'age' }), /no history/);
    assert.match(sparkline({ values: [1], label: 'age' }), /no history/);
    assert.match(sparkline({ values: [1, 2, 3], label: 'age' }), /polyline/);
});

test('stateChip always carries its word alongside any colour', () => {
    assert.match(stateChip('FAULT', 'bad'), />FAULT</);
    assert.match(stateChip('FAULT', 'bad'), /w-bad/);
});

test('knobValueFromPointer maps the pointer to a snapped value', () => {
    // Straight up is the middle of the sweep.
    assert.equal(knobValueFromPointer(0, 4095, 1, 0, -10), 2048);
    // Right and left are past the middle, on the correct side.
    assert.ok(knobValueFromPointer(0, 4095, 1, 10, 0) > 2048);
    assert.ok(knobValueFromPointer(0, 4095, 1, -10, 0) < 2048);
    // Straight down is the dead zone and clamps to the maximum.
    assert.equal(knobValueFromPointer(0, 4095, 1, 0, 10), 4095);
});

test('knobValueFromPointer snaps to the step and stays in range', () => {
    for (let i = -20; i <= 20; i++) {
        const v = knobValueFromPointer(0, 359, 1, i, -10);
        assert.ok(v >= 0 && v <= 359);
        assert.equal(v, Math.round(v));
    }
    assert.equal(knobValueFromPointer(0, 100, 5, 0, -10), 50);
});

/* ---------------------------------------------------------------------------
   The camera shot result
   --------------------------------------------------------------------------- */

/* The camera panel renders the size of a photograph, what it costs on the link,
 * and whether the driver gave us the resolution we asked for. Each is a fact the
 * operator cannot get anywhere else at the moment they press the button.
 *
 * The tests below are mostly about the three-way absence, which is where a
 * rendering rule goes wrong silently: a "—" and a "0×0" both look like code that
 * ran, and only one of them is true. */
test('the shot result names the size and what it costs on the link', () => {
    const out = renderShotResult('1280x720', {
        size_bytes: 218_440,
        width: 1280,
        height: 720,
        fetch_seconds: 16,
    });
    assert.match(out, /640×480|1280×720/);
    assert.match(out, /KiB|MiB/);
    /* A span, rendered as one. Not a clock time: clockText answers "what time
     * will it be", so a 16-second fetch rendered as a wall clock reads as a time
     * of day and means something else entirely. */
    assert.match(out, /0:16 to fetch/);
    assert.doesNotMatch(out, /to fetch<\/|am|pm/);
});

test('the shot result says when the driver clamped the resolution', () => {
    const out = renderShotResult('1920x1080', {
        size_bytes: 40_000,
        width: 640,
        height: 480,
        fetch_seconds: 3,
    });
    assert.match(out, /clamped/);
    assert.match(out, /1920x1080 was asked for/);
    assert.match(out, /640×480/);
});

test('the shot result says nothing about clamping when the request was met', () => {
    const out = renderShotResult('1280x720', {
        size_bytes: 40_000,
        width: 1280,
        height: 720,
        fetch_seconds: 3,
    });
    assert.doesNotMatch(out, /clamped/);
});

/* Three different facts, three different renderings. The middle one is the one
 * that gets lost: a photograph exists, its header just could not be read, and
 * drawing 0×0 for that is a claim about a picture nobody took. */
test('the shot result keeps "no size decoded" apart from a real size', () => {
    const unreadable = renderShotResult('1280x720', {
        size_bytes: 40_000,
        width: null,
        height: null,
        fetch_seconds: 3,
    });
    assert.match(unreadable, /size unknown/);
    assert.doesNotMatch(unreadable, /0×0/);

    const known = renderShotResult('1280x720', {
        size_bytes: 40_000,
        width: 640,
        height: 480,
        fetch_seconds: 3,
    });
    assert.match(known, /640×480/);
    assert.doesNotMatch(known, /size unknown/);
});

/* An unknown link rate means the fetch cost is unknown, and the line omits it
 * rather than claiming zero seconds. */
test('the shot result omits the fetch cost when the link rate is unknown', () => {
    const out = renderShotResult(null, {
        size_bytes: 40_000,
        width: 640,
        height: 480,
        fetch_seconds: 0,
    });
    assert.doesNotMatch(out, /to fetch/);
    assert.match(out, /640×480/);
});

/* A command that produced no artefact renders nothing at all -- not an empty
 * string that leaves a stray divider, and not a zero. */
test('the shot result is empty when there is no artefact', () => {
    assert.equal(renderShotResult('1280x720', null), '');
});
