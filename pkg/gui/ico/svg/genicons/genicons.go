// Copyright 2016 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"bytes"
	"encoding/xml"
	"flag"
	"fmt"
	"go/format"
	"image/color"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"golang.org/x/exp/shiny/iconvg"
	"golang.org/x/image/math/f32"
)

var outDir = flag.String("o", "", "output directory")
var pkgName = flag.String("pkg", "icons", "package name")

var (
	out      = new(bytes.Buffer)
	failures = []string{}
	varNames = []string{}

	totalFiles    int
	totalIVGBytes int
	totalSVGBytes int
)

func upperCase(s string) string {
	if c := s[0]; 'a' <= c && c <= 'z' {
		return string(c-0x20) + s[1:]
	}
	return s
}

func main() {
	flag.Parse()
	args := flag.Args()
	if len(args) < 1 {
		fmt.Fprintf(os.Stderr, "please provide a directory to convert\n")
		os.Exit(2)
	}
	iconsDir := args[0]
	out.WriteString("//go:generate go run genicons/genicons.go genicons/log.go -pkg p9icons . \n")
	out.WriteString("// generated by go run gen.go; DO NOT EDIT\n\npackage ")
	out.WriteString(*pkgName)
	out.WriteString("\n\n")
	if err := genDir(iconsDir); err != nil {
		Fatal(err)
	}
	fmt.Fprintf(out,
		"// In total, %d SVG bytes in %d files converted to %d IconVG bytes.\n",
		totalSVGBytes, totalFiles, totalIVGBytes)
	if len(failures) != 0 {
		out.WriteString("\n/*\nFAILURES:\n\n")
		for _, failure := range failures {
			out.WriteString(failure)
			out.WriteByte('\n')
		}
		out.WriteString("\n*/")
	}
	if *outDir != "" {
		if err := os.MkdirAll(*outDir, 0775); err != nil && !os.IsExist(err) {
			Fatal(err)
		}
	}
	raw := out.Bytes()
	formatted, err := format.Source(raw)
	if err != nil {
		Fatalf("gofmt failed: %v\n\nGenerated code:\n%s", err, raw)
	}
	// formatted := raw
	if err := ioutil.WriteFile(filepath.Join(*outDir, "data.go"), formatted, 0644); err != nil {
		Fatalf("WriteFile failed: %s\n", err)
	}
	{
		b := new(bytes.Buffer)
		b.WriteString("// generated by go run genicons.go; DO NOT EDIT\n\npackage ")
		b.WriteString(*pkgName)
		b.WriteString("\n\n")
		b.WriteString("var list = []struct{ name string; data []byte } {\n")
		for _, v := range varNames {
			fmt.Fprintf(b, "{%q, %s},\n", v, v)
		}
		b.WriteString("}\n\n")
		raw := b.Bytes()
		formatted, err := format.Source(raw)
		if err != nil {
			Fatalf("gofmt failed: %v\n\nGenerated code:\n%s", err, raw)
		}
		if err := ioutil.WriteFile(filepath.Join(*outDir, "data_test.go"), formatted, 0644); err != nil {
			Fatalf("WriteFile failed: %s\n", err)
		}
	}
}

func genDir(dirName string) error {
	fqSVGDirName := filepath.FromSlash(dirName)
	f, err := os.Open(fqSVGDirName)
	if err != nil {
		return err
	}
	defer f.Close()

	infos, err := f.Readdir(-1)
	if err != nil {
		Fatal(err)
	}
	baseNames, fileNames, sizes := []string{}, map[string]string{}, map[string]int{}
	for _, info := range infos {
		name := info.Name()

		nameParts := strings.Split(name, "_")
		if len(nameParts) != 3 || nameParts[0] != "ic" {
			continue
		}
		baseName := nameParts[1]
		var size int
		if n, err := fmt.Sscanf(nameParts[2], "%dpx.svg", &size); err != nil || n != 1 {
			continue
		}
		if prevSize, ok := sizes[baseName]; ok {
			if size > prevSize {
				fileNames[baseName] = name
				sizes[baseName] = size
			}
		} else {
			fileNames[baseName] = name
			sizes[baseName] = size
			baseNames = append(baseNames, baseName)
		}
	}

	sort.Strings(baseNames)
	for _, baseName := range baseNames {
		fileName := fileNames[baseName]
		path := filepath.Join(dirName, fileName)
		f, err := ioutil.ReadFile(path)
		if err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", path, err))
			continue
		}
		if err = genFile(f, baseName, float32(sizes[baseName])); err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", path, err))
			continue
		}
	}
	return nil
}

type SVG struct {
	Width   string  `xml:"width,attr"`
	Height  string  `xml:"height,attr"`
	Fill    string  `xml:"fill,attr"`
	ViewBox string  `xml:"viewBox,attr"`
	Paths   []*Path `xml:"path"`
	// Some of the SVG files contain <circle> elements, not just <path>
	// elements. IconVG doesn't have circles per se. Instead, we convert such
	// circles to paired arcTo commands, tacked on to the first path.
	//
	// In general, this isn't correct if the circles and the path overlap, but
	// that doesn't happen in the specific case of the Material Design icons.
	Circles []Circle `xml:"circle"`
}

type Path struct {
	D           string   `xml:"d,attr"`
	Fill        string   `xml:"fill,attr"`
	FillOpacity *float32 `xml:"fill-opacity,attr"`
	Opacity     *float32 `xml:"opacity,attr"`

	creg uint8
}

type Circle struct {
	Cx float32 `xml:"cx,attr"`
	Cy float32 `xml:"cy,attr"`
	R  float32 `xml:"r,attr"`
}

func genFile(svgData []byte, baseName string, outSize float32) error {
	var varName string
	for _, s := range strings.Split(baseName, "_") {
		varName += upperCase(s)
	}
	fmt.Fprintf(out, "var %s = []byte{", varName)
	defer fmt.Fprintf(out, "\n}\n\n")
	varNames = append(varNames, varName)

	g := &SVG{}
	if err := xml.Unmarshal(svgData, g); err != nil {
		return err
	}

	var vbx, vby, vbx2, vby2 float32
	for i, v := range strings.Split(g.ViewBox, " ") {
		f, err := strconv.ParseFloat(v, 32)
		if err != nil {
			return fmt.Errorf("genFile: failed to parse ViewBox (%q): %v",
				g.ViewBox, err)
		}
		switch i {
		case 0:
			vbx = float32(f)
		case 1:
			vby = float32(f)
		case 2:
			vbx2 = float32(f)
		case 3:
			vby2 = float32(f)
		}
	}
	dx, dy := outSize, outSize
	var size float32
	if aspect := (vbx2 - vbx) / (vby2 - vby); aspect >= 1 {
		dy /= aspect
		size = vbx2 - vbx
	} else {
		dx /= aspect
		size = vby2 - vby
	}
	palette := iconvg.DefaultPalette
	pmap := make(map[color.RGBA]uint8)
	for _, p := range g.Paths {
		if p.Fill == "" {
			p.Fill = g.Fill
		}
		c, err := parseColor(p.Fill)
		if err != nil {
			return err
		}
		var ok bool
		if p.creg, ok = pmap[c]; !ok {
			if len(pmap) == 64 {
				panic("too many colors")
			}
			p.creg = uint8(len(pmap))
			palette[p.creg] = c
			pmap[c] = p.creg
		}
	}
	var enc iconvg.Encoder
	enc.Reset(iconvg.Metadata{
		ViewBox: iconvg.Rectangle{
			Min: f32.Vec2{-dx * .5, -dy * .5},
			Max: f32.Vec2{+dx * .5, +dy * .5},
		},
		Palette: palette,
	})

	offset := f32.Vec2{
		vbx * outSize / size,
		vby * outSize / size,
	}

	// adjs maps from opacity to a cReg adj value.
	adjs := map[float32]uint8{}

	for _, p := range g.Paths {
		if err := genPath(&enc, p, adjs, outSize, size, offset, g.Circles); err != nil {
			return err
		}
		g.Circles = nil
	}

	if len(g.Circles) != 0 {
		if err := genPath(&enc, &Path{}, adjs, outSize, size, offset, g.Circles); err != nil {
			return err
		}
		g.Circles = nil
	}

	ivgData, err := enc.Bytes()
	if err != nil {
		return fmt.Errorf("iconvg encoding failed: %v", err)
	}
	for i, x := range ivgData {
		if i&0x0f == 0x00 {
			out.WriteByte('\n')
		}
		fmt.Fprintf(out, "%#02x, ", x)
	}

	totalFiles++
	totalSVGBytes += len(svgData)
	totalIVGBytes += len(ivgData)
	return nil
}

func parseColor(col string) (color.RGBA, error) {
	if col == "none" {
		return color.RGBA{}, nil
	}
	if len(col) == 0 {
		return color.RGBA{A: 0xff}, nil
	}
	if len(col) == 0 || col[0] != '#' {
		return color.RGBA{}, fmt.Errorf("invalid color: %q", col)
	}
	col = col[1:]
	if len(col) != 6 {
		return color.RGBA{}, fmt.Errorf("invalid color length: %q", col)
	}
	elems := make([]byte, len(col)/2)
	for i := range elems {
		e, err := strconv.ParseUint(col[i*2:i*2+2], 16, 8)
		if err != nil {
			return color.RGBA{}, err
		}
		elems[i] = byte(e)
	}
	return color.RGBA{R: elems[0], G: elems[1], B: elems[2], A: 255}, nil
}

func genPath(enc *iconvg.Encoder, p *Path, adjs map[float32]uint8, outSize, size float32, offset f32.Vec2, circles []Circle) error {
	adj := uint8(0)
	opacity := float32(1)
	if p.Opacity != nil {
		opacity = *p.Opacity
	} else if p.FillOpacity != nil {
		opacity = *p.FillOpacity
	}
	if opacity != 1 {
		var ok bool
		if adj, ok = adjs[opacity]; !ok {
			adj = uint8(len(adjs) + 1)
			adjs[opacity] = adj
			// Set CREG[0-adj] to be a blend of transparent (0x7f) and the
			// first custom palette color (0x80).
			enc.SetCReg(adj, false, iconvg.BlendColor(uint8(opacity*0xff), 0x7f, 0x80+p.creg))
		}
	} else {
		enc.SetCReg(adj, false, iconvg.PaletteIndexColor(p.creg))
	}

	needStartPath := true
	if p.D != "" {
		needStartPath = false
		if err := genPathData(enc, adj, p.D, outSize, size, offset); err != nil {
			return err
		}
	}

	for _, c := range circles {
		// Normalize.
		cx := c.Cx * outSize / size
		cx -= outSize/2 + offset[0]
		cy := c.Cy * outSize / size
		cy -= outSize/2 + offset[1]
		r := c.R * outSize / size

		if needStartPath {
			needStartPath = false
			enc.StartPath(adj, cx-r, cy)
		} else {
			enc.ClosePathAbsMoveTo(cx-r, cy)
		}

		// Convert a circle to two relative arcTo ops, each of 180 degrees.
		// We can't use one 360 degree arcTo as the start and end point
		// would be coincident and the computation is degenerate.
		enc.RelArcTo(r, r, 0, false, true, +2*r, 0)
		enc.RelArcTo(r, r, 0, false, true, -2*r, 0)
	}

	enc.ClosePathEndPath()
	return nil
}

func genPathData(enc *iconvg.Encoder, adj uint8, pathData string, outSize, size float32, offset f32.Vec2) error {
	if strings.HasSuffix(pathData, "z") {
		pathData = pathData[:len(pathData)-1]
	}
	r := strings.NewReader(pathData)

	var args [7]float32
	op, relative, started := byte(0), false, false
	var count int
	for {
		b, err := r.ReadByte()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		count++

		switch {
		case b == ' ' || b == '\n' || b == '\t':
			continue
		case 'A' <= b && b <= 'Z':
			op, relative = b, false
		case 'a' <= b && b <= 'z':
			op, relative = b, true
		default:
			r.UnreadByte()
		}

		n := 0
		switch op {
		case 'A', 'a':
			n = 7
		case 'L', 'l', 'T', 't':
			n = 2
		case 'Q', 'q', 'S', 's':
			n = 4
		case 'C', 'c':
			n = 6
		case 'H', 'h', 'V', 'v':
			n = 1
		case 'M', 'm':
			n = 2
		case 'Z', 'z':
		default:
			return fmt.Errorf("unknown opcode %c\n", b)
		}

		scan(&args, r, n)
		normalize(&args, n, op, outSize, size, offset, relative)

		switch op {
		case 'A':
			enc.AbsArcTo(args[0], args[1], args[2], args[3] != 0, args[4] != 0, args[5], args[6])
		case 'a':
			enc.RelArcTo(args[0], args[1], args[2], args[3] != 0, args[4] != 0, args[5], args[6])
		case 'L':
			enc.AbsLineTo(args[0], args[1])
		case 'l':
			enc.RelLineTo(args[0], args[1])
		case 'T':
			enc.AbsSmoothQuadTo(args[0], args[1])
		case 't':
			enc.RelSmoothQuadTo(args[0], args[1])
		case 'Q':
			enc.AbsQuadTo(args[0], args[1], args[2], args[3])
		case 'q':
			enc.RelQuadTo(args[0], args[1], args[2], args[3])
		case 'S':
			enc.AbsSmoothCubeTo(args[0], args[1], args[2], args[3])
		case 's':
			enc.RelSmoothCubeTo(args[0], args[1], args[2], args[3])
		case 'C':
			enc.AbsCubeTo(args[0], args[1], args[2], args[3], args[4], args[5])
		case 'c':
			enc.RelCubeTo(args[0], args[1], args[2], args[3], args[4], args[5])
		case 'H':
			enc.AbsHLineTo(args[0])
		case 'h':
			enc.RelHLineTo(args[0])
		case 'V':
			enc.AbsVLineTo(args[0])
		case 'v':
			enc.RelVLineTo(args[0])
		case 'M':
			if !started {
				started = true
				enc.StartPath(adj, args[0], args[1])
			} else {
				enc.ClosePathAbsMoveTo(args[0], args[1])
			}
		case 'm':
			enc.ClosePathRelMoveTo(args[0], args[1])
		}
	}
	return nil
}

func scan(args *[7]float32, r *strings.Reader, n int) {
	for i := 0; i < n; i++ {
		for {
			if b, _ := r.ReadByte(); b != ' ' && b != ',' && b != '\n' && b != '\t' {
				r.UnreadByte()
				break
			}
		}
		fmt.Fscanf(r, "%f", &args[i])
	}
}

func normalize(args *[7]float32, n int, op byte, outSize, size float32, offset f32.Vec2, relative bool) {
	for i := 0; i < n; i++ {
		if (op == 'A' || op == 'a') && (i == 3 || i == 4) {
			continue
		}
		args[i] *= outSize / size
		if relative {
			continue
		}
		if (op == 'A' || op == 'a') && i < 5 {
			// For arcs, skip everything other than x, y.
			continue
		}
		args[i] -= outSize / 2
		switch {
		case op == 'A' && i == 5: // Arc x.
			args[i] -= offset[0]
		case op == 'A' && i == 6: // Arc y.
			args[i] -= offset[1]
		case n != 1:
			args[i] -= offset[i&0x01]
		case op == 'H':
			args[i] -= offset[0]
		case op == 'V':
			args[i] -= offset[1]
		}
	}
}
