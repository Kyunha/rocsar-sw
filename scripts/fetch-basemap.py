#!/usr/bin/env python3
"""Fetch the Natural Earth 50m basemap as GeoJSON for the Ground Station map.

The map (cmd/gs/frontend/src/map.ts) renders a low-detail offline basemap from
Natural Earth. This script downloads the upstream GeoJSON, reduces it, and
writes pre-compressed assets into cmd/gs/frontend/public/geojson/.

Why a script rather than committing the upstream files directly:

  - Natural Earth ships full float64 repr coordinates (14-15 decimal places),
    which is roughly 1 cm precision on a dataset whose linework is generalised
    at a 50 million scale. Rounding to 6 dp (~11 cm) loses nothing visible and
    removes a large fraction of the bytes, before gzip.
  - geography_regions_polys carries 514 features of which only 127 are mountain
    ranges. Shipping island groups, continents and deserts to draw rivers,
    lakes and mountains would be paying for 3 MB of map we never render, so the
    file is filtered to featurecla == "Range/mtn".
  - The assets are pre-gzipped and served with Content-Encoding: gzip
    (cmd/gs/bridge.go), so the bytes embedded in the Go binary are the
    compressed ones.

The output is committed, like the generated protobuf stubs, so the build works
with no network. Re-run this only when the Natural Earth data changes.

Usage:
    scripts/fetch-basemap.py [--out DIR] [--dp N]

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

BASE_URL = (
    "https://raw.githubusercontent.com/martynafford/natural-earth-geojson/"
    "master/50m/physical/"
)
DEFAULT_OUT = os.path.join(
    os.path.dirname(os.path.dirname(os.path.abspath(__file__))),
    "cmd", "gs", "frontend", "public", "geojson",
)

# Every layer the map draws, in draw order (later layers paint on top).
# "filter" is an optional (property, value) predicate applied to features.
LAYERS = [
    {"name": "ne_50m_ocean"},
    {"name": "ne_50m_land"},
    {"name": "ne_50m_lakes"},
    {"name": "ne_50m_rivers_lake_centerlines"},
    {
        "name": "ne_50m_geography_regions_polys",
        "filter": ("featurecla", "Range/mtn"),
        "as": "ne_50m_mountain_ranges",
    },
    {"name": "ne_50m_geography_regions_elevation_points"},
]


def fetch(name: str) -> dict:
    url = BASE_URL + name + ".json"
    req = urllib.request.Request(url, headers={"User-Agent": "rocsar-basemap/1.0"})
    with urllib.request.urlopen(req, timeout=120) as resp:
        return json.loads(resp.read().decode("utf-8"))


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


def transform(doc: dict, layer: dict, nd: int) -> dict:
    features = doc["features"]
    flt = layer.get("filter")
    if flt is not None:
        prop, want = flt
        features = [f for f in features if f.get("properties", {}).get(prop) == want]
    features = [round_floats(f, nd) for f in features]
    return {"type": "FeatureCollection", "features": features}


def write(path: str, doc: dict) -> tuple[int, int]:
    raw = json.dumps(doc, separators=(",", ":")).encode("utf-8")
    gz = gzip.compress(raw, compresslevel=9, mtime=0)
    with open(path + ".json.gz", "wb") as f:
        f.write(gz)
    return len(raw), len(gz)


def main() -> int:
    ap = argparse.ArgumentParser(description="Fetch the Natural Earth 50m basemap.")
    ap.add_argument("--out", default=DEFAULT_OUT, help="output directory")
    ap.add_argument("--dp", type=int, default=6, help="coordinate decimal places")
    args = ap.parse_args()

    os.makedirs(args.out, exist_ok=True)

    total_raw = total_gz = 0
    print(f"{'layer':<40} {'features':>8} {'raw':>10} {'gzip':>10}")
    for layer in LAYERS:
        name = layer.get("as", layer["name"])
        src = fetch(layer["name"])
        doc = transform(src, layer, args.dp)
        out = os.path.join(args.out, name)
        raw, gz = write(out, doc)
        total_raw += raw
        total_gz += gz
        print(f"{name:<40} {len(doc['features']):>8} {raw:>10} {gz:>10}")

    print(f"{'TOTAL':<40} {'':>8} {total_raw:>10} {total_gz:>10}")
    print(f"\nwrote {len(LAYERS)} assets to {args.out}", file=sys.stderr)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
