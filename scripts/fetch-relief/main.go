// Command fetch-relief builds the Ground Station's topographic basemap: a
// hypsometric tint (colour by elevation) combined with shaded relief, cropped to
// the operating area, reprojected to Web Mercator, darkened into the console's
// palette, and written as a single georeferenced JPEG that the map renders as a
// MapLibre image source.
//
// Three things here are not obvious and all are load-bearing.
//
// Why a baked raster rather than DEM terrain tiles: measured against the AWS
// Terrarium bucket, a z0..z7 pyramid over the same box is 26.7 MiB, and a DEM
// fine enough to beat this image is more, whereas this crop is a fraction of a
// megabyte. See GUI_ARCHITECTURE.md 1.2.1.
//
// Why two sources: a shaded-relief raster (SR_HR) encodes which way a slope
// faces, not how high it is -- a sunlit valley floor and a shadowed 2 000 m
// ridge can share a luminance. It therefore cannot be turned into elevation
// bands no matter how the colour ramp is drawn. HYP_HR is Natural Earth's
// hypsometric tint, which *is* colour by elevation (teal lowlands, khaki
// uplands, white peaks). The render takes its hue from HYP and its shading from
// SR, so the map shows both what is high and what is steep.
//
// Why the reprojection: Natural Earth rasters are plate carree (equal degrees),
// while MapLibre draws in Web Mercator. An image source is placed by mapping its
// four corners to their Mercator tile coordinates and interpolating linearly
// across the quad (maplibre-gl src/source/image_source.ts) -- there is no
// projection transform. Over 22 degrees of latitude the difference between
// linear-in-latitude and linear-in-Mercator-y reaches ~2 degrees (~200 km) at
// the midpoint, which would put the Scandinavian mountains in the North Sea. A
// Mercator-projected image with its corners on parallels and meridians maps
// exactly through that same quad, so the warp is done once here instead.
//
// Why the colour is baked rather than a MapLibre raster paint property:
// raster-hue-rotate needs saturation to rotate, and a greyscale hillshade has
// none, so the client cannot tint it into the dark palette. Rasterizing here is
// both cheaper and better looking.
//
// Natural Earth is public domain and requires no attribution.
//
// Usage:
//
//	go run ./scripts/fetch-relief
//	go run ./scripts/fetch-relief --no-tint        # old greyscale hillshade
//	go run ./scripts/fetch-relief --product 50m
//	go run ./scripts/fetch-relief --zip /tmp/SR_HR.zip --tint-zip /tmp/HYP_HR.zip
package main

import (
	"archive/zip"
	"flag"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"golang.org/x/image/tiff"
)

// product is one Natural Earth raster release. The pixel density is derived
// from the decoded image size, not stored here, so a mislabelled product cannot
// silently mis-crop.
type product struct {
	url     string
	tifName string // file inside the zip
}

// pairing is the two Natural Earth rasters the render needs: the hypsometric
// tint (`tint`) that carries elevation as colour, and the shaded relief
// (`shade`) that carries slope as luminance.
//
// 50m has no standalone hypsometric tint in the Natural Earth release --
// HYP_50M_SR is the tint with shading already baked in -- so its `shade` is
// empty and the render shades from the tint image's own luminance. That is
// self-consistent (one source, one shading) and only affects the low-resolution
// path; the shipped asset is 10m.
type pairing struct {
	tint  product
	shade product
}

var products = map[string]pairing{
	// 21 600 x 10 800, 1/60 degree per pixel -- the detailed one.
	"10m": {
		tint:  product{"https://naturalearth.s3.amazonaws.com/10m_raster/HYP_HR.zip", "HYP_HR.tif"},
		shade: product{"https://naturalearth.s3.amazonaws.com/10m_raster/SR_HR.zip", "SR_HR.tif"},
	},
	// 10 800 x 5 400, 1/30 degree per pixel -- a quarter of the pixels,
	// hypsometric tint and shading combined in one image.
	"50m": {
		tint: product{"https://naturalearth.s3.amazonaws.com/50m_raster/HYP_50M_SR.zip", "HYP_50M_SR.tif"},
	},
}

// tintGain darkens the hypsometric tint into the console palette. The measured
// tint over this box runs from teal lowlands (#799f99) through khaki (#ddccaa)
// to white peaks; multiplying by this keeps lowlands near the panel's land
// colour, leaves peaks clearly lighter, and holds the whole basemap below the
// cyan and amber command accents (GUI_ARCHITECTURE.md 1.2.1).
const tintGain = 0.30

// shadeBaseline is the measured luminance of flat ground in SR_HR, so the
// shading multiplier is 1.0 on the plains and the tint is neither darkened nor
// lifted overall. The clamps stop a single extreme slope from driving a pixel to
// black or blowing it out.
const (
	shadeBaseline = 203.0
	shadeMin      = 0.60
	shadeMax      = 1.45
)

// The hypsometric source has no water: its ocean is exactly #ffffff while every
// land feature stays tinted, so near-white is painted straight to the console's
// ocean colour. This is deliberately the same value as the map's ocean layer
// (map.ts), so the sea is seamless whether the vector fill covers it or not.
const (
	waterR, waterG, waterB = 0x14, 0x20, 0x2c
	waterCut               = 250
)

// bbox is the crop, in degrees.
type bbox struct {
	west, south, east, north float64
}

func (b bbox) String() string {
	return fmt.Sprintf("%g,%g,%g,%g", b.west, b.south, b.east, b.north)
}

// rampStop is one node of the greyscale colour ramp used by --no-tint. `at` is
// the input luminance (0..255), so the stops can be placed against the actual
// range of the relief rather than spread over the full 0..255, most of which
// this data never uses.
//
// The measured Northern Europe relief sits at a baseline of ~203 (ocean and flat
// plains are nearly the same value), with terrain deviating to ~64 in shadow and
// ~247 on sunlit ridges. The stops below put flat ground near the console's land
// colour and let ridges ride up out of it. They carry no elevation information
// and the default path no longer uses them; they remain so the old look is
// reproducible for comparison.
type rampStop struct {
	at      float64
	r, g, b uint8
}

var ramp = []rampStop{
	{0, 10, 12, 15},
	{120, 17, 20, 25},
	{200, 28, 33, 41},
	{225, 52, 58, 67},
	{255, 100, 104, 100},
}

// buildLUT expands the ramp into a 256-entry lookup table, clamped at the ends.
func buildLUT() [256]color.RGBA {
	var lut [256]color.RGBA
	for i := 0; i < 256; i++ {
		t := float64(i)
		lo, hi := ramp[0], ramp[len(ramp)-1]
		for j := 0; j < len(ramp)-1; j++ {
			if t >= ramp[j].at && t <= ramp[j+1].at {
				lo, hi = ramp[j], ramp[j+1]
				break
			}
		}
		f := 0.0
		if hi.at > lo.at {
			f = (t - lo.at) / (hi.at - lo.at)
		}
		if f < 0 {
			f = 0
		} else if f > 1 {
			f = 1
		}
		lerp := func(a, b uint8) uint8 {
			return uint8(float64(a) + (float64(b)-float64(a))*f + 0.5)
		}
		lut[i] = color.RGBA{lerp(lo.r, hi.r), lerp(lo.g, hi.g), lerp(lo.b, hi.b), 255}
	}
	return lut
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "fetch-relief: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	product := flag.String("product", "10m", "Natural Earth relief product: 10m or 50m")
	outDir := flag.String("out", filepath.Join("cmd", "gs", "frontend", "public", "relief"), "output directory")
	outName := flag.String("name", "ne_relief.jpg", "output file name")
	localZip := flag.String("zip", "", "use a local shaded-relief .zip instead of downloading")
	tintZip := flag.String("tint-zip", "", "use a local hypsometric-tint .zip instead of downloading")
	noTint := flag.Bool("no-tint", false, "emit the old greyscale hillshade instead of the hypsometric tint")
	quality := flag.Int("quality", 90, "JPEG quality, 1-100")
	bboxStr := flag.String("bbox", "-12,50,45,72", "crop: west,south,east,north in degrees")
	flag.Parse()
	tinted := !*noTint

	p, ok := products[*product]
	if !ok {
		return fmt.Errorf("unknown product %q (want 10m or 50m)", *product)
	}
	box, err := parseBBox(*bboxStr)
	if err != nil {
		return err
	}

	// The hypsometric tint carries the colour; the shaded relief carries the
	// slope. When the pairing has no separate relief (50m), the tint image is
	// used for both, so it is loaded once.
	reliefSrc := p.shade
	if reliefSrc.url == "" {
		reliefSrc = p.tint
	}
	reliefImg, err := load(reliefSrc, *localZip)
	if err != nil {
		return err
	}
	tintImg := reliefImg
	if p.tint.url != reliefSrc.url {
		tintImg, err = load(p.tint, *tintZip)
		if err != nil {
			return err
		}
	}
	if tintImg.Bounds() != reliefImg.Bounds() {
		return fmt.Errorf("source grids differ: tint %v, relief %v",
			tintImg.Bounds(), reliefImg.Bounds())
	}
	imgBounds := tintImg.Bounds()
	fmt.Printf("source %d x %d px\n", imgBounds.Dx(), imgBounds.Dy())

	// The product is plate carree covering -180..180, -90..90, so the pixel
	// density is the image size divided by the span. Deriving it here rather
	// than hardcoding 60 or 30 means a wrong -product value cannot produce a
	// crop that is silently off by a factor of two.
	pxPerLon := float64(imgBounds.Dx()) / 360
	pxPerLat := float64(imgBounds.Dy()) / 180

	r := image.Rect(
		int((box.west+180)*pxPerLon+0.5),
		int((90-box.north)*pxPerLat+0.5),
		int((box.east+180)*pxPerLon+0.5),
		int((90-box.south)*pxPerLat+0.5),
	).Intersect(imgBounds)
	if r.Empty() {
		return fmt.Errorf("bbox %s falls outside the image", box)
	}
	fmt.Printf("crop   %s -> %d,%d..%d,%d (%d x %d px, plate carree)\n",
		box, r.Min.X, r.Min.Y, r.Max.X, r.Max.Y, r.Dx(), r.Dy())
	histogram(reliefImg, r)
	if tinted {
		paletteReport(tintImg, r)
	}

	out := render(tintImg, reliefImg, r, box, tinted)
	fmt.Printf("warp   Web Mercator -> %d x %d px\n", out.Bounds().Dx(), out.Bounds().Dy())

	if err := os.MkdirAll(*outDir, 0o755); err != nil {
		return err
	}
	path := filepath.Join(*outDir, *outName)
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := jpeg.Encode(f, out, &jpeg.Options{Quality: *quality}); err != nil {
		return err
	}
	if st, err := f.Stat(); err == nil {
		fmt.Printf("wrote  %s (%d bytes, q%d)\n", path, st.Size(), *quality)
	}
	return nil
}

// load returns the decoded TIFF, from a local zip if given, else the network.
func load(p product, localZip string) (image.Image, error) {
	zipPath := localZip
	if zipPath == "" {
		cached, err := download(p.url)
		if err != nil {
			return nil, err
		}
		zipPath = cached
	}

	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", zipPath, err)
	}
	defer zr.Close()

	for _, f := range zr.File {
		if !strings.EqualFold(filepath.Base(f.Name), p.tifName) {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return nil, err
		}
		defer rc.Close()
		img, err := tiff.Decode(rc)
		if err != nil {
			return nil, fmt.Errorf("decode %s: %w", f.Name, err)
		}
		return img, nil
	}
	return nil, fmt.Errorf("%s not found in %s", p.tifName, zipPath)
}

// download returns the path to the zip, downloading it into a per-user cache on
// first use. The products are 40-90 MB and the render is re-run while tuning, so
// paying for the transfer once matters; the cache is keyed by URL basename and
// written via a temp file and rename so an interrupted download cannot leave a
// truncated zip behind to be decoded as if complete.
func download(url string) (string, error) {
	dir, err := os.UserCacheDir()
	if err != nil {
		dir = os.TempDir()
	}
	dir = filepath.Join(dir, "rocsar-relief")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	dst := filepath.Join(dir, filepath.Base(url))
	if st, err := os.Stat(dst); err == nil && st.Size() > 0 {
		fmt.Printf("cached %s (%d bytes)\n", dst, st.Size())
		return dst, nil
	}

	resp, err := http.Get(url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	tmp, err := os.CreateTemp(dir, filepath.Base(url)+".*")
	if err != nil {
		return "", err
	}
	if _, err := io.Copy(tmp, resp.Body); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return "", err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return "", err
	}
	if err := os.Rename(tmp.Name(), dst); err != nil {
		os.Remove(tmp.Name())
		return "", err
	}
	fmt.Printf("downloaded %s\n", url)
	return dst, nil
}

// mercY returns the Web Mercator y in [0,1] for a latitude in degrees.
func mercY(latDeg float64) float64 {
	lat := latDeg * math.Pi / 180
	return (1 - math.Log(math.Tan(math.Pi/4+lat/2))/math.Pi) / 2
}

// invMercY returns the latitude in degrees for a Web Mercator y in [0,1].
func invMercY(y float64) float64 {
	return (2*math.Atan(math.Exp(math.Pi*(1-2*y))) - math.Pi/2) * 180 / math.Pi
}

// render crops the plate-carree sources to the box and resamples them onto a Web
// Mercator grid in a single pass, combining elevation colour with shading.
//
// Longitude is linear in both projections, so only rows are resampled: each
// output row's Mercator y is inverted to a latitude and looked up in the
// sources. The output height is chosen to hold the horizontal resolution
// constant in Mercator space, which is what makes the stretched upper latitudes
// keep their detail instead of being stretched from fewer rows.
//
// With tinted set, each pixel is the hypsometric colour scaled by tintGain (into
// the console palette) and by a shading factor taken from the relief's luminance
// relative to flat ground. Without it, the old greyscale ramp is used and the
// relief alone drives the colour.
func render(tint, relief image.Image, r image.Rectangle, box bbox, tinted bool) *image.RGBA {
	lut := buildLUT()
	w := r.Dx()
	yTop, yBot := mercY(box.north), mercY(box.south)
	h := int(float64(w)*(yBot-yTop)*360/(box.east-box.west) + 0.5)

	out := image.NewRGBA(image.Rect(0, 0, w, h))
	latSpan := box.north - box.south
	rows := float64(r.Dy())
	sameImg := tint == relief

	for j := 0; j < h; j++ {
		my := yTop + (float64(j)+0.5)/float64(h)*(yBot-yTop)
		lat := invMercY(my)
		sy := r.Min.Y + int((box.north-lat)/latSpan*rows)
		if sy < r.Min.Y {
			sy = r.Min.Y
		}
		if sy >= r.Max.Y {
			sy = r.Max.Y - 1
		}
		for i := 0; i < w; i++ {
			x := r.Min.X + i
			if !tinted {
				g := color.GrayModel.Convert(relief.At(x, sy)).(color.Gray)
				out.SetRGBA(i, j, lut[g.Y])
				continue
			}

			tr, tg, tb, _ := tint.At(x, sy).RGBA()
			r8, g8, b8 := uint8(tr>>8), uint8(tg>>8), uint8(tb>>8)

			// HYP_HR carries no water layer: its ocean is exactly #ffffff.
			// Measurement shows every land feature -- glaciers and the highest
			// peaks included -- stays tinted (brightest ~#d5c3ac), so near-white
			// is unambiguously water. Painting it here keeps the raster correct
			// on its own rather than depending on the vector ocean layer to
			// cover a flat grey sea.
			if r8 >= waterCut && g8 >= waterCut && b8 >= waterCut {
				out.SetRGBA(i, j, color.RGBA{waterR, waterG, waterB, 255})
				continue
			}

			// Shading is the relief's luminance relative to flat ground. When
			// the tint image is also the relief (50m), its own luminance is all
			// there is.
			var luma uint8
			if sameImg {
				luma = color.GrayModel.Convert(color.RGBA{r8, g8, b8, 255}).(color.Gray).Y
			} else {
				luma = color.GrayModel.Convert(relief.At(x, sy)).(color.Gray).Y
			}
			shade := float64(luma) / shadeBaseline
			if shade < shadeMin {
				shade = shadeMin
			} else if shade > shadeMax {
				shade = shadeMax
			}

			f := tintGain * shade
			out.SetRGBA(i, j, color.RGBA{
				scale(r8, f),
				scale(g8, f),
				scale(b8, f),
				255,
			})
		}
	}
	return out
}

// scale multiplies a colour channel by f, saturating rather than wrapping.
func scale(v uint8, f float64) uint8 {
	s := float64(v) * f
	if s > 255 {
		s = 255
	}
	return uint8(s + 0.5)
}

// paletteReport prints the dominant colours of the hypsometric crop, quantised
// to 4 bits per channel. This is the measurement behind tintGain and the band
// description in GUI_ARCHITECTURE.md 1.2.1; re-run it if the source release
// changes, rather than eyeballing a palette that may have moved.
func paletteReport(img image.Image, r image.Rectangle) {
	m := map[uint32]int{}
	for y := r.Min.Y; y < r.Max.Y; y++ {
		for x := r.Min.X; x < r.Max.X; x++ {
			cr, cg, cb, _ := img.At(x, y).RGBA()
			m[uint32(cr>>12)<<8|uint32(cg>>12)<<4|uint32(cb>>12)]++
		}
	}
	type bucket struct {
		key   uint32
		count int
	}
	list := make([]bucket, 0, len(m))
	for c, n := range m {
		list = append(list, bucket{c, n})
	}
	sort.Slice(list, func(i, j int) bool { return list[i].count > list[j].count })
	total := r.Dx() * r.Dy()
	fmt.Println("tint   dominant hypsometric colours (>=0.5%):")
	for _, e := range list {
		pct := 100 * float64(e.count) / float64(total)
		if pct < 0.5 {
			break
		}
		r8 := uint8((e.key>>8)&0xf) * 17
		g8 := uint8((e.key>>4)&0xf) * 17
		b8 := uint8(e.key&0xf) * 17
		l := color.GrayModel.Convert(color.RGBA{r8, g8, b8, 255}).(color.Gray).Y
		fmt.Printf("         ~#%02x%02x%02x %6.1f%% luma %3d\n", r8, g8, b8, pct, l)
	}
}

// histogram prints the luminance spread of the crop. A crop concentrated in one
// bin means the relief is flat there and the ramp has nothing to work with,
// which is worth knowing before blaming the renderer.
func histogram(img image.Image, r image.Rectangle) {
	var bins [8]int
	for y := r.Min.Y; y < r.Max.Y; y++ {
		for x := r.Min.X; x < r.Max.X; x++ {
			g := color.GrayModel.Convert(img.At(x, y)).(color.Gray)
			bins[int(g.Y)/32]++
		}
	}
	total := r.Dx() * r.Dy()
	fmt.Printf("luma   ")
	for i, n := range bins {
		fmt.Printf("%d:%4.1f%% ", i*32, 100*float64(n)/float64(total))
	}
	fmt.Println()
}

func parseBBox(s string) (bbox, error) {
	parts := strings.Split(s, ",")
	if len(parts) != 4 {
		return bbox{}, fmt.Errorf("bbox %q: want west,south,east,north", s)
	}
	v := make([]float64, 4)
	for i, p := range parts {
		f, err := strconv.ParseFloat(strings.TrimSpace(p), 64)
		if err != nil {
			return bbox{}, fmt.Errorf("bbox %q: %w", s, err)
		}
		v[i] = f
	}
	b := bbox{v[0], v[1], v[2], v[3]}
	if b.west >= b.east || b.south >= b.north {
		return bbox{}, fmt.Errorf("bbox %s: want west<east and south<north", b)
	}
	return b, nil
}
