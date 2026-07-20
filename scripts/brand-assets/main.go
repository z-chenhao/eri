// Command brand-assets derives Eri's transparent web brand assets from the
// user-provided source illustration without redrawing the artwork.
package main

import (
	"fmt"
	"image"
	"image/color"
	_ "image/jpeg"
	"image/png"
	"math"
	"os"
	"path/filepath"
)

const sourcePath = "assets/brand/source/eri-portrait-original.jpg"

type output struct {
	path string
	size int
}

var outputs = []output{
	{path: "assets/brand/eri-mark.png", size: 1024},
	{path: "assets/brand/eri-icon-512.png", size: 512},
	{path: "assets/brand/eri-icon-192.png", size: 192},
	{path: "assets/brand/eri-favicon-32.png", size: 32},
	{path: "web/conversation/brand/eri-mark.png", size: 1024},
	{path: "web/conversation/brand/eri-icon-512.png", size: 512},
	{path: "web/conversation/brand/eri-icon-192.png", size: 192},
	{path: "web/conversation/brand/eri-favicon-32.png", size: 32},
	{path: "web/observatory/brand/eri-mark.png", size: 1024},
	{path: "web/observatory/brand/eri-favicon-32.png", size: 32},
}

func main() {
	file, err := os.Open(sourcePath)
	if err != nil {
		fatalf("open source: %v", err)
	}
	defer file.Close()

	source, _, err := image.Decode(file)
	if err != nil {
		fatalf("decode source: %v", err)
	}
	mark := removeExteriorWhite(source)
	for _, target := range outputs {
		asset := resize(mark, target.size, target.size)
		if err := writePNG(target.path, asset); err != nil {
			fatalf("write %s: %v", target.path, err)
		}
	}
}

// removeExteriorWhite only visits near-white pixels connected to the canvas
// boundary. White areas enclosed by the emblem stay opaque, including Eri's
// face, clothing, flower, and the star inset.
func removeExteriorWhite(source image.Image) *image.NRGBA {
	bounds := source.Bounds()
	width, height := bounds.Dx(), bounds.Dy()
	mark := image.NewNRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			mark.Set(x, y, source.At(bounds.Min.X+x, bounds.Min.Y+y))
		}
	}

	outside := make([]bool, width*height)
	queue := make([]image.Point, 0, 2*(width+height))
	seed := func(x, y int) {
		index := y*width + x
		if outside[index] || !nearWhite(mark.NRGBAAt(x, y)) {
			return
		}
		outside[index] = true
		queue = append(queue, image.Pt(x, y))
	}
	for x := 0; x < width; x++ {
		seed(x, 0)
		seed(x, height-1)
	}
	for y := 1; y < height-1; y++ {
		seed(0, y)
		seed(width-1, y)
	}

	directions := [...]image.Point{image.Pt(1, 0), image.Pt(-1, 0), image.Pt(0, 1), image.Pt(0, -1)}
	for head := 0; head < len(queue); head++ {
		point := queue[head]
		for _, direction := range directions {
			x, y := point.X+direction.X, point.Y+direction.Y
			if x < 0 || x >= width || y < 0 || y >= height {
				continue
			}
			seed(x, y)
		}
	}

	// Pull the matte two pixels into the antialiased outer edge. Pure black
	// outline pixels stop the expansion, so the enclosed white artwork remains.
	matte := append([]bool(nil), outside...)
	for range 2 {
		next := append([]bool(nil), matte...)
		for y := 0; y < height; y++ {
			for x := 0; x < width; x++ {
				if matte[y*width+x] {
					continue
				}
				pixel := mark.NRGBAAt(x, y)
				if !neutral(pixel) || luminance(pixel) <= 12 {
					continue
				}
				for _, direction := range directions {
					nx, ny := x+direction.X, y+direction.Y
					if nx >= 0 && nx < width && ny >= 0 && ny < height && matte[ny*width+nx] {
						next[y*width+x] = true
						break
					}
				}
			}
		}
		matte = next
	}

	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			index := y*width + x
			if outside[index] {
				mark.SetNRGBA(x, y, color.NRGBA{})
				continue
			}
			if !matte[index] {
				continue
			}
			coverage := uint8(255 - luminance(mark.NRGBAAt(x, y)))
			mark.SetNRGBA(x, y, color.NRGBA{A: coverage})
		}
	}
	return mark
}

func nearWhite(pixel color.NRGBA) bool {
	return neutral(pixel) && luminance(pixel) >= 180
}

func neutral(pixel color.NRGBA) bool {
	minimum := min(pixel.R, pixel.G, pixel.B)
	maximum := max(pixel.R, pixel.G, pixel.B)
	return maximum-minimum <= 20
}

func luminance(pixel color.NRGBA) uint8 {
	return uint8((299*uint32(pixel.R) + 587*uint32(pixel.G) + 114*uint32(pixel.B)) / 1000)
}

func resize(source image.Image, width, height int) *image.NRGBA {
	bounds := source.Bounds()
	target := image.NewNRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		sy := (float64(y)+0.5)*float64(bounds.Dy())/float64(height) - 0.5
		y0 := int(math.Floor(sy))
		fy := sy - float64(y0)
		for x := 0; x < width; x++ {
			sx := (float64(x)+0.5)*float64(bounds.Dx())/float64(width) - 0.5
			x0 := int(math.Floor(sx))
			fx := sx - float64(x0)
			target.SetNRGBA(x, y, bilinear(source, bounds, x0, y0, fx, fy))
		}
	}
	return target
}

func bilinear(source image.Image, bounds image.Rectangle, x, y int, fx, fy float64) color.NRGBA {
	weights := [4]float64{(1 - fx) * (1 - fy), fx * (1 - fy), (1 - fx) * fy, fx * fy}
	points := [4]image.Point{image.Pt(x, y), image.Pt(x+1, y), image.Pt(x, y+1), image.Pt(x+1, y+1)}
	var red, green, blue, alpha float64
	for index, point := range points {
		point.X = min(max(point.X, 0), bounds.Dx()-1) + bounds.Min.X
		point.Y = min(max(point.Y, 0), bounds.Dy()-1) + bounds.Min.Y
		r, g, b, a := source.At(point.X, point.Y).RGBA()
		weight := weights[index]
		red += float64(r) * weight
		green += float64(g) * weight
		blue += float64(b) * weight
		alpha += float64(a) * weight
	}
	if alpha <= 0 {
		return color.NRGBA{}
	}
	return color.NRGBA{
		R: uint8(math.Round(red * 255 / alpha)),
		G: uint8(math.Round(green * 255 / alpha)),
		B: uint8(math.Round(blue * 255 / alpha)),
		A: uint8(math.Round(alpha / 257)),
	}
}

func writePNG(path string, asset image.Image) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	file, err := os.Create(path)
	if err != nil {
		return err
	}
	if err := png.Encode(file, asset); err != nil {
		file.Close()
		return err
	}
	return file.Close()
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
