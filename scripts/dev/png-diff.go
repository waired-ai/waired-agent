//go:build ignore

// Command png-diff computes a per-channel mean absolute difference
// between two PNG images of identical bounds, in 0..65535 units
// (image/color.RGBA64). Used in Phase 8.5 visual verification to
// objectively confirm which reference icon (waired-{connected,
// degraded,disconnected,error}.png) the tray was rendering at each
// captured checkpoint, without relying on subjective screenshot
// inspection.
//
// Output line: "A vs B: diff_per_channel=N max_per_pixel=M"
// where diff_per_channel is the mean of |Aᵢ - Bᵢ| across all R,G,B,A
// channels of every pixel, and max_per_pixel is the worst single-pixel
// sum of channel-wise absolute differences. A near-zero diff means
// the two PNGs are pixel-identical (allowing for PNG re-encoding via
// SNI's ARGB32 round-trip).
//
// Exit 0 on success, 2 on size mismatch / decode error.
package main

import (
	"fmt"
	"image"
	_ "image/png"
	"os"
)

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: png-diff A.png B.png")
		os.Exit(2)
	}
	a := loadPNG(os.Args[1])
	b := loadPNG(os.Args[2])
	if a.Bounds() != b.Bounds() {
		fmt.Printf("size mismatch: %s=%v vs %s=%v\n",
			os.Args[1], a.Bounds(), os.Args[2], b.Bounds())
		os.Exit(2)
	}
	var (
		sum    int64
		maxPix int64
	)
	w, h := a.Bounds().Dx(), a.Bounds().Dy()
	for y := range h {
		for x := range w {
			ar, ag, ab, aa := a.At(x, y).RGBA()
			br, bg, bb, ba := b.At(x, y).RGBA()
			d := abs64(int64(ar)-int64(br)) +
				abs64(int64(ag)-int64(bg)) +
				abs64(int64(ab)-int64(bb)) +
				abs64(int64(aa)-int64(ba))
			sum += d
			if d > maxPix {
				maxPix = d
			}
		}
	}
	pixels := int64(w * h)
	fmt.Printf("%s vs %s: diff_per_channel=%d max_per_pixel=%d (pixels=%d)\n",
		os.Args[1], os.Args[2], sum/(pixels*4), maxPix, pixels)
}

func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

func loadPNG(path string) image.Image {
	f, err := os.Open(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, "open:", err)
		os.Exit(2)
	}
	defer f.Close()
	img, _, err := image.Decode(f)
	if err != nil {
		fmt.Fprintln(os.Stderr, "decode:", path, err)
		os.Exit(2)
	}
	return img
}
