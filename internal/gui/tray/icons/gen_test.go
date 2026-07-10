package icons

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestGenerateIcons regenerates the four tray icons under
// internal/gui/tray/icons/. It is gated behind WAIRED_TRAY_REGEN so a
// regular `go test ./...` run does not rewrite the committed PNGs.
//
// Run manually with: WAIRED_TRAY_REGEN=1 go test ./internal/gui/tray/icons/
//
// Each icon is the WAIRED "GATE" brand mark — two facing arcs around a
// core dot, "( ● )" — drawn at ~22 px so it sits cleanly in tray pixel
// grids on both GNOME (StatusNotifierItem icon) and KDE. The four
// states are distinguished by colour (and a lit vs hollow core); see
// drawGate. Re-run after design tweaks; the resulting waired-*.png /
// .ico files are what go:embed in icons_unix.go / icons_windows.go
// consumes.
func TestGenerateIcons(t *testing.T) {
	if os.Getenv("WAIRED_TRAY_REGEN") == "" {
		t.Skip("set WAIRED_TRAY_REGEN=1 to regenerate the tray icons")
	}
	dir := "."
	const size = 22
	const ss = 4 // supersample factor for anti-aliasing (render at size*ss, box-downsample)

	// Brand palette (matches web/admin/.../Logo.tsx GATE): arcs in
	// brand blue, lit core in cyan. The off/error states recolour the
	// whole mark.
	arcConnected := color.RGBA{0x8f, 0xbd, 0xf0, 0xff}  // #8fbdf0 brand blue
	coreConnected := color.RGBA{0x7f, 0xe9, 0xff, 0xff} // #7fe9ff cyan
	muted := color.RGBA{0x88, 0x88, 0x88, 0xff}         // #888888 grey
	errorFG := color.RGBA{0xd9, 0x4c, 0x4c, 0xff}       // #d94c4c red
	warning := color.RGBA{0xff, 0xc6, 0x2a, 0xff}       // #ffc62a yellow

	type entry struct {
		name string
		img  image.Image
	}
	entries := []entry{
		// Connected: the full lit GATE — brand-blue arcs + cyan core.
		{"waired-connected.png", drawGate(size, ss, arcConnected, coreConnected, true)},
		// Disconnected: same shape drained to grey, core hollow ("off").
		{"waired-disconnected.png", drawGate(size, ss, muted, muted, false)},
		// Error: red GATE.
		{"waired-error.png", drawGate(size, ss, errorFG, errorFG, true)},
		// Degraded: connected GATE + small yellow warning triangle in
		// the upper-right corner. Used when the network tunnel is up
		// but the wrapper-side gating reports the Claude integration
		// as unreachable (silent-breakage avoidance).
		{"waired-degraded.png", overlayWarning(drawGate(size, ss, arcConnected, coreConnected, true), warning)},
	}
	for _, e := range entries {
		buf := bytes.Buffer{}
		if err := png.Encode(&buf, e.img); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, e.name), buf.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
		// Emit a matching .ico for the Windows tray backend.
		// fyne.io/systray on Windows requires .ico bytes (per its
		// godoc for SetIcon); macOS and Linux backends accept PNG
		// directly so we keep the PNGs above too.
		icoName := strings.TrimSuffix(e.name, ".png") + ".ico"
		icoBytes, err := pngBytesToICO(buf.Bytes(), size)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, icoName), icoBytes, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// pngBytesToICO wraps a PNG payload in an ICO container. Vista+ ICO
// readers accept PNG-encoded entries directly (no BMP DIB required),
// which keeps this helper to ~30 lines of straight binary writes.
//
// The ICO file layout is:
//
//	ICONDIR     header     (6 bytes)
//	ICONDIRENTRY * count   (16 bytes each)
//	image data * count     (raw PNG bytes here)
//
// Width/height fields are zero when the actual dimension is >= 256;
// our 22 px icons fit in the byte so we write the dimension directly.
func pngBytesToICO(pngBytes []byte, dim int) ([]byte, error) {
	if dim >= 256 {
		dim = 0 // ICO convention: 0 means "256 or larger"
	}
	var buf bytes.Buffer
	// ICONDIR: reserved=0, type=1 (ICO), count=1.
	_ = binary.Write(&buf, binary.LittleEndian, uint16(0))
	_ = binary.Write(&buf, binary.LittleEndian, uint16(1))
	_ = binary.Write(&buf, binary.LittleEndian, uint16(1))
	// ICONDIRENTRY:
	//   width (1), height (1), colors (1), reserved (1),
	//   planes (2), bitcount (2), bytesinres (4), imageoffset (4).
	buf.WriteByte(byte(dim))
	buf.WriteByte(byte(dim))
	buf.WriteByte(0)                                        // colors: 0 for >= 256-color image
	buf.WriteByte(0)                                        // reserved
	_ = binary.Write(&buf, binary.LittleEndian, uint16(1))  // planes
	_ = binary.Write(&buf, binary.LittleEndian, uint16(32)) // bpp
	_ = binary.Write(&buf, binary.LittleEndian, uint32(len(pngBytes)))
	_ = binary.Write(&buf, binary.LittleEndian, uint32(6+16)) // offset right after the header
	buf.Write(pngBytes)
	return buf.Bytes(), nil
}

// overlayWarning paints a small yellow triangle (with a darker
// outline) in the upper-right of base. Standalone helper so the
// composition is reviewable in isolation.
func overlayWarning(base image.Image, fill color.RGBA) image.Image {
	b := base.Bounds()
	out := image.NewRGBA(b)
	draw.Draw(out, b, base, b.Min, draw.Src)

	// Triangle bounding box: 9×9 pixels in the upper-right corner.
	const tri = 9
	originX := b.Max.X - tri - 1
	originY := b.Min.Y + 1
	outline := color.RGBA{0x33, 0x33, 0x33, 0xff}
	for y := 0; y < tri; y++ {
		// Filled triangle: at row y, draw from (tri-1-y)/2 .. (tri-1+y)/2.
		left := (tri - 1 - y) / 2
		right := (tri - 1 + y) / 2
		for x := left; x <= right; x++ {
			px := originX + x
			py := originY + y
			if x == left || x == right || y == tri-1 {
				out.Set(px, py, outline)
			} else {
				out.Set(px, py, fill)
			}
		}
	}
	// "!" mark inside the triangle: a dark vertical stroke + dot.
	cx := originX + (tri-1)/2
	for dy := 3; dy <= 5; dy++ {
		out.Set(cx, originY+dy, outline)
	}
	out.Set(cx, originY+7, outline)
	return out
}

// drawGate renders the WAIRED GATE mark — two facing stroked arcs plus
// a core dot, "( ● )" — at `size` px. It rasterises on an `ss`×
// supersampled canvas and box-downsamples for anti-aliasing, since
// pure-Go per-pixel arc edges are otherwise jagged at tray resolution.
//
// arc is the stroke colour of the two gateway arcs; core is the colour
// of the centre dot; coreFilled selects a solid dot (lit, connected)
// vs a hollow ring (dimmed, disconnected).
//
// Geometry reproduces web/admin/src/components/icons/Logo.tsx, scaled
// from its 64-unit viewBox:
//
//   - Arc 1 (right "(": SVG "M40,15 A18,18 0 0 1 40,49") has circle
//     centre C1=(34.084,32); it is the minor arc, bulging right through
//     the +x point. Arc 2 ("M24,49 A18,18 0 0 1 24,15") mirrors it with
//     centre C2=(29.916,32), bulging left through −x. Both centres sit
//     d=√(R²−h²)=√35≈5.916 from the chord (R=18, half-chord h=17), and
//     each arc spans ±atan2(17,√35) ≈ ±70.8° about its bulge direction.
//   - The web stroke is 3.4/64; at 22 px that is ~1.2 px and nearly
//     vanishes in the tray grid, so the icon variant thickens it to
//     5/64 (the user approved an icon-legibility tweak).
//   - Core: a filled (or hollow) circle of radius 6.4/64.
func drawGate(size, ss int, arc, core color.RGBA, coreFilled bool) image.Image {
	S := size * ss
	img := image.NewRGBA(image.Rect(0, 0, S, S)) // zero value = transparent
	f := float64(S) / 64.0

	const strokeUnits = 5.0
	half := (strokeUnits / 2.0) * f // half stroke width
	R := 18.0 * f
	coreR := 6.4 * f
	cx, cy := 32.0*f, 32.0*f

	c1x, c1y := 34.084*f, 32.0*f // right-bulging ")" arc centre
	c2x, c2y := 29.916*f, 32.0*f // left-bulging "(" arc centre
	halfAng := math.Atan2(17.0, math.Sqrt(35.0))

	// Round-cap centres (the four arc endpoints, scaled). A filled disk
	// of radius `half` at each reproduces strokeLinecap="round".
	caps := [4][2]float64{
		{40 * f, 15 * f}, {40 * f, 49 * f}, // arc 1 endpoints
		{24 * f, 49 * f}, {24 * f, 15 * f}, // arc 2 endpoints
	}

	inArc := func(px, py, ccx, ccy, centreAng float64) bool {
		dx, dy := px-ccx, py-ccy
		if d := math.Hypot(dx, dy); math.Abs(d-R) > half {
			return false
		}
		return math.Abs(angDiff(math.Atan2(dy, dx), centreAng)) <= halfAng
	}

	for y := 0; y < S; y++ {
		for x := 0; x < S; x++ {
			px, py := float64(x)+0.5, float64(y)+0.5

			// Core dot (does not overlap the arcs, so handled first).
			if dr := math.Hypot(px-cx, py-cy); dr <= coreR {
				if coreFilled || dr >= coreR-2.4*f {
					img.SetRGBA(x, y, core)
				}
				continue
			}

			// Two arcs (bulging ±x) + their round caps.
			painted := inArc(px, py, c1x, c1y, 0) || inArc(px, py, c2x, c2y, math.Pi)
			for i := 0; !painted && i < len(caps); i++ {
				painted = math.Hypot(px-caps[i][0], py-caps[i][1]) <= half
			}
			if painted {
				img.SetRGBA(x, y, arc)
			}
		}
	}
	return downsample(img, size, ss)
}

// angDiff returns the signed difference a−b wrapped into (−π, π], so an
// arc's angular span can be tested as |angDiff(θ, centre)| ≤ halfWidth
// without special-casing the ±π wraparound (the left arc straddles it).
func angDiff(a, b float64) float64 {
	d := math.Mod(a-b, 2*math.Pi)
	if d <= -math.Pi {
		d += 2 * math.Pi
	} else if d > math.Pi {
		d -= 2 * math.Pi
	}
	return d
}

// downsample box-averages each ss×ss block of src into one output pixel,
// in premultiplied-alpha space so transparent edges don't bleed dark.
//
// The result is an *image.NRGBA (straight / non-premultiplied alpha): we
// recover straight RGB via unpremul, and image.NRGBA is the container
// image/png writes verbatim. (Storing straight values into an
// image.RGBA instead would corrupt them — png.Encode treats RGBA as
// premultiplied and un-premultiplies on write, inflating edge colours.)
func downsample(src *image.RGBA, size, ss int) image.Image {
	out := image.NewNRGBA(image.Rect(0, 0, size, size))
	n := float64(ss * ss)
	for oy := 0; oy < size; oy++ {
		for ox := 0; ox < size; ox++ {
			var pr, pg, pb, sa float64
			for dy := 0; dy < ss; dy++ {
				for dx := 0; dx < ss; dx++ {
					c := src.RGBAAt(ox*ss+dx, oy*ss+dy)
					af := float64(c.A) / 255.0
					pr += float64(c.R) * af
					pg += float64(c.G) * af
					pb += float64(c.B) * af
					sa += float64(c.A)
				}
			}
			avgA := sa / n
			out.SetNRGBA(ox, oy, color.NRGBA{
				R: unpremul(pr/n, avgA),
				G: unpremul(pg/n, avgA),
				B: unpremul(pb/n, avgA),
				A: clamp8(avgA),
			})
		}
	}
	return out
}

// unpremul converts an averaged premultiplied channel back to straight
// alpha given the averaged alpha (0..255); returns 0 for fully empty.
func unpremul(premul, avgA float64) uint8 {
	if avgA <= 0 {
		return 0
	}
	return clamp8(premul * 255.0 / avgA)
}

func clamp8(v float64) uint8 {
	switch {
	case v <= 0:
		return 0
	case v >= 255:
		return 255
	default:
		return uint8(v + 0.5)
	}
}
