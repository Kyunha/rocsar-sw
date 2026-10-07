#!/usr/bin/env python3
"""Fetch the Natural Earth basemap as GeoJSON for the Ground Station map.

The map (cmd/gs/frontend/src/map.ts) renders a low-detail offline basemap from
Natural Earth. This script downloads the upstream GeoJSON, merges and reduces
it, and writes pre-compressed assets into cmd/gs/frontend/public/geojson/.

Why a script rather than committing the upstream files directly:

  - Natural Earth ships full float64 repr coordinates (14-15 decimal places),
    which is roughly 1 cm precision on data whose linework is generalised at a
    10-50 million scale. Rounding to 6 dp (~11 cm) loses nothing visible and
    removes a large fraction of the bytes, before gzip.
  - geography_regions_polys carries 514 features of which only 127 are mountain
    ranges. Shipping island groups, continents and deserts to draw mountains
    would be paying for megabytes of map we never render, so the file is
    filtered to featurecla == "Range/mtn".
  - The assets are pre-gzipped and served with Content-Encoding: gzip
    (cmd/gs/bridge.go), so the bytes embedded in the Go binary are the
    compressed ones.

Why some layers merge two upstream files: Natural Earth publishes its denser
European hydrography as *supplements* which exclude the features already in the
global set. ne_10m_lakes_europe has no Ladoga and no Vanern; ne_10m_rivers_europe
has no Rhine and no Danube. Either source alone is incomplete, so the global set
and the supplement are merged. At 50m there are 58 lakes in the whole of
Northern Europe (Finland alone has some 188,000); at 10m plus supplement there
are several hundred.

The output is committed, like the generated protobuf stubs, so the build works
with no network. Re-run this only when the Natural Earth data changes. Raw
downloads are cached under ~/.cache/rocsar-basemap, because the 10m hydrography
is ~23 MB across four files and re-fetching it on every run is pointless.

Usage:
    scripts/fetch-basemap.py [--out DIR] [--dp N] [--refresh]

Natural Earth is public domain (https://www.naturalearthdata.com/about/terms-of-use/).
The GeoJSON mirror is github.com/martynafford/natural-earth-geojson.
"""

from __future__ import annotations

import argparse
import gzip
import json
import os
import sys
import urllib.request

MIRROR = (
    "https://raw.githubusercontent.com/martynafford/"
    "natural-earth-geojson/master/{scale}/physical/"
)
CACHE_DIR = os.path.join(os.path.expanduser("~"), ".cache", "rocsar-basemap")
DEFAULT_OUT = os.path.join(
    os.path.dirname(os.path.dirname(os.path.abspath(__file__))),
    "cmd", "gs", "frontend", "public", "geojson",
)

# The operating area, as (west, south, east, north). Layers marked "clip" keep
# only features whose bounding box intersects this, which is what keeps the 10m
# hydrography to a few hundred kilobytes instead of several megabytes of water
# nobody in the operating area will look at. It matches the relief image's box
# (scripts/fetch-relief). The base land/ocean/mountain layers stay global.
NE_BOX = (-12.0, 50.0, 45.0, 72.0)

# Every layer the map draws. Each entry is one output file, built from one or
# more (scale, name) upstream sources merged in order. "filter" is an optional
# (property, value) predicate applied to every source's features; "clip" limits
# the output to the operating area.
LAYERS = [
    {"as": "ne_50m_ocean", "sources": [("50m", "ne_50m_ocean")]},
    {"as": "ne_50m_land", "sources": [("50m", "ne_50m_land")]},
    {
        "as": "ne_lakes",
        "sources": [("10m", "ne_10m_lakes"), ("10m", "ne_10m_lakes_europe")],
        "clip": NE_BOX,
    },
    {
        "as": "ne_rivers",
        "sources": [
            ("10m", "ne_10m_rivers_lake_centerlines"),
            ("10m", "ne_10m_rivers_europe"),
        ],
        "clip": NE_BOX,
    },
    {
        "as": "ne_50m_mountain_ranges",
        "sources": [("50m", "ne_50m_geography_regions_polys")],
        "filter": ("featurecla", "Range/mtn"),
    },
    {
        "as": "ne_50m_geography_regions_elevation_points",
        "sources": [("50m", "ne_50m_geography_regions_elevation_points")],
    },
]


def fetch(scale: str, name: str, refresh: bool) -> dict:
    """Return one upstream GeoJSON, from the cache when possible."""
    os.makedirs(CACHE_DIR, exist_ok=True)
    cached = os.path.join(CACHE_DIR, f"{scale}_{name}.json")
    if not refresh and os.path.exists(cached):
        with open(cached, "rb") as f:
            return json.loads(f.read().decode("utf-8"))

    url = MIRROR.format(scale=scale) + name + ".json"
    req = urllib.request.Request(url, headers={"User-Agent": "rocsar-basemap/1.0"})
    with urllib.request.urlopen(req, timeout=300) as resp:
        data = resp.read()
    with open(cached, "wb") as f:
        f.write(data)
    return json.loads(data.decode("utf-8"))


def round_floats(value, nd: int):
    """Recursively round every float to `nd` decimal places.

    Coordinates are nested lists of floats; properties are scalars, strings and
    bools. Rounding is safe to apply to the whole document because the only
    floats are coordinates (and the elevation/min_zoom numbers, which do not
    care about the extra digits either).
    """
    if isinstance(value, float):
        return round(value, nd)
    if isinstance(value, list):
        return [round_floats(v, nd) for v in value]
    if isinstance(value, dict):
        return {k: round_floats(v, nd) for k, v in value.items()}
    return value


def feature_bbox(geom: dict) -> tuple[float, float, float, float]:
    xs: list[float] = []
    ys: list[float] = []

    def walk(c) -> None:
        if isinstance(c[0], (int, float)):
            xs.append(c[0])
            ys.append(c[1])
        else:
            for part in c:
                walk(part)

    walk(geom["coordinates"])
    return min(xs), min(ys), max(xs), max(ys)


def intersects(geom: dict, box: tuple[float, float, float, float]) -> bool:
    """True when a feature's bounding box overlaps the box.

    Bounding-box overlap, not a geometric clip: a feature that crosses the edge
    is kept whole. For hydrography that is the behaviour we want — a river
    leaving the area should still run off the edge of the map rather than stop
    at an invisible line — and it avoids carrying a polygon clipper.
    """
    w, s, e, n = box
    if not geom or "coordinates" not in geom:
        return False
    try:
        x0, y0, x1, y1 = feature_bbox(geom)
    except (KeyError, IndexError, ValueError, TypeError):
        return False
    return x0 <= e and x1 >= w and y0 <= n and y1 >= s


def transform(layer: dict, nd: int, refresh: bool) -> tuple[dict, int]:
    feats: list = []
    for scale, name in layer["sources"]:
        doc = fetch(scale, name, refresh)
        fs = doc["features"]
        flt = layer.get("filter")
        if flt is not None:
            prop, want = flt
            fs = [f for f in fs if f.get("properties", {}).get(prop) == want]
        clip = layer.get("clip")
        if clip is not None:
            fs = [f for f in fs if intersects(f["geometry"], clip)]
        feats.extend(round_floats(f, nd) for f in fs)
    return {"type": "FeatureCollection", "features": feats}, len(layer["sources"])


def write(path: str, doc: dict) -> tuple[int, int]:
    raw = json.dumps(doc, separators=(",", ":")).encode("utf-8")
    gz = gzip.compress(raw, compresslevel=9, mtime=0)
    with open(path + ".json.gz", "wb") as f:
        f.write(gz)
    return len(raw), len(gz)


def main() -> int:
    ap = argparse.ArgumentParser(description="Fetch the Natural Earth basemap.")
    ap.add_argument("--out", default=DEFAULT_OUT, help="output directory")
    ap.add_argument("--dp", type=int, default=6, help="coordinate decimal places")
    ap.add_argument("--refresh", action="store_true", help="ignore the download cache")
    args = ap.parse_args()

    os.makedirs(args.out, exist_ok=True)

    total_raw = total_gz = 0
    print(f"{'layer':<46} {'sources':>7} {'features':>8} {'raw':>10} {'gzip':>10}")
    for layer in LAYERS:
        doc, nsrc = transform(layer, args.dp, args.refresh)
        out = os.path.join(args.out, layer["as"])
        raw, gz = write(out, doc)
        total_raw += raw
        total_gz += gz
        print(f"{layer['as']:<46} {nsrc:>7} {len(doc['features']):>8} {raw:>10} {gz:>10}")

    print(f"{'TOTAL':<46} {'':>7} {'':>8} {total_raw:>10} {total_gz:>10}")
    print(f"\nwrote {len(LAYERS)} assets to {args.out}", file=sys.stderr)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
