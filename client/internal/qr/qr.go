// Package qr encodes text as a QR code (ISO/IEC 18004) for a phone's camera
// to read: byte mode, error correction level M, versions 1 to 10, which hold
// an address of up to 213 bytes. It draws the code for a terminal and as SVG.
package qr

import (
	"errors"
	"fmt"
	"strings"
)

// Code is an encoded QR code: Size by Size modules, dark or light.
type Code struct {
	size     int
	dark     [][]bool
	function [][]bool // finder, timing, alignment, format and version modules
}

// The tables of level M for versions 1 to 10: error correction codewords
// per block, and blocks.
var (
	eccPerBlock = [...]int{0, 10, 16, 26, 18, 24, 16, 18, 22, 22, 26}
	blocks      = [...]int{0, 1, 1, 1, 2, 2, 4, 4, 4, 5, 5}
)

const maxVersion = 10

// Encode returns the smallest code that holds text.
func Encode(text string) (*Code, error) { return encode([]byte(text), -1) }

func encode(data []byte, mask int) (*Code, error) {
	version := 0
	for v := 1; v <= maxVersion; v++ {
		if 4+countBits(v)+8*len(data) <= 8*dataCodewords(v) {
			version = v
			break
		}
	}
	if version == 0 {
		return nil, fmt.Errorf("%d bytes do not fit a QR code of version %d", len(data), maxVersion)
	}
	// Byte mode, the count, the bytes, a terminator, and padding.
	var bits bitBuffer
	bits.append(0b0100, 4)
	bits.append(len(data), countBits(version))
	for _, b := range data {
		bits.append(int(b), 8)
	}
	capacity := 8 * dataCodewords(version)
	bits.append(0, min(4, capacity-len(bits)))
	bits.append(0, (8-len(bits)%8)%8)
	for pad := 0xEC; len(bits) < capacity; pad ^= 0xEC ^ 0x11 {
		bits.append(pad, 8)
	}
	codewords := make([]byte, len(bits)/8)
	for i, bit := range bits {
		if bit {
			codewords[i>>3] |= 1 << (7 - i&7)
		}
	}

	c := &Code{size: 4*version + 17}
	c.dark, c.function = grid(c.size), grid(c.size)
	c.drawFunctions(version)
	c.drawCodewords(interleave(version, codewords))
	if mask < 0 {
		best := -1
		for m := range 8 {
			c.applyMask(m)
			c.drawFormat(m)
			if p := c.penalty(); best < 0 || p < best {
				best, mask = p, m
			}
			c.applyMask(m)
		}
	}
	c.applyMask(mask)
	c.drawFormat(mask)
	return c, nil
}

// Size is the code's width and height in modules, without its quiet zone.
func (c *Code) Size() int { return c.size }

// Dark reports whether the module at column x, row y is dark.
func (c *Code) Dark(x, y int) bool {
	return x >= 0 && y >= 0 && x < c.size && y < c.size && c.dark[y][x]
}

// quiet is the light margin a scanner needs around a code, in modules.
const quiet = 4

// Terminal draws the code two rows to a line with half blocks, dark on a
// light background whatever the terminal's colors.
func (c *Code) Terminal() string {
	var sb strings.Builder
	for y := -quiet; y < c.size+quiet; y += 2 {
		sb.WriteString("\x1b[30;107m")
		for x := -quiet; x < c.size+quiet; x++ {
			switch top, bottom := c.Dark(x, y), c.Dark(x, y+1); {
			case top && bottom:
				sb.WriteString("█")
			case top:
				sb.WriteString("▀")
			case bottom:
				sb.WriteString("▄")
			default:
				sb.WriteByte(' ')
			}
		}
		sb.WriteString("\x1b[0m\n")
	}
	return sb.String()
}

// SVG draws the code with its quiet zone, a module to a unit.
func (c *Code) SVG() string {
	n := c.size + 2*quiet
	var path strings.Builder
	for y := range c.size {
		for x := range c.size {
			if c.dark[y][x] {
				fmt.Fprintf(&path, "M%d %dh1v1h-1z", x+quiet, y+quiet)
			}
		}
	}
	return fmt.Sprintf(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 %d %d" shape-rendering="crispEdges">`+
		`<rect width="%d" height="%d" fill="#fff"/><path fill="#000" d="%s"/></svg>`, n, n, n, n, path.String())
}

// countBits is the width of byte mode's character count.
func countBits(version int) int {
	if version < 10 {
		return 8
	}
	return 16
}

// rawModules is how many modules of a version carry codewords.
func rawModules(version int) int {
	n := (16*version+128)*version + 64
	if version >= 2 {
		align := version/7 + 2
		n -= (25*align-10)*align - 55
		if version >= 7 {
			n -= 36
		}
	}
	return n
}

func dataCodewords(version int) int {
	return rawModules(version)/8 - eccPerBlock[version]*blocks[version]
}

// interleave splits the data into the version's blocks, adds each block's
// error correction, and takes them column by column.
func interleave(version int, data []byte) []byte {
	numBlocks, ecc := blocks[version], eccPerBlock[version]
	raw := rawModules(version) / 8
	short := numBlocks - raw%numBlocks
	shortLen := raw / numBlocks
	divisor := rsDivisor(ecc)
	var parts [][]byte
	k := 0
	for i := range numBlocks {
		n := shortLen - ecc
		if i >= short {
			n++
		}
		block := append([]byte(nil), data[k:k+n]...)
		k += n
		rem := rsRemainder(block, divisor)
		if i < short {
			block = append(block, 0) // a short block's gap, skipped below
		}
		parts = append(parts, append(block, rem...))
	}
	var out []byte
	for i := range parts[0] {
		for j, p := range parts {
			if i != shortLen-ecc || j >= short {
				out = append(out, p[i])
			}
		}
	}
	return out
}

func (c *Code) set(x, y int, dark bool) {
	c.dark[y][x] = dark
	c.function[y][x] = true
}

func (c *Code) drawFunctions(version int) {
	for i := range c.size {
		c.set(6, i, i%2 == 0)
		c.set(i, 6, i%2 == 0)
	}
	for _, at := range [][2]int{{3, 3}, {c.size - 4, 3}, {3, c.size - 4}} {
		for dy := -4; dy <= 4; dy++ {
			for dx := -4; dx <= 4; dx++ {
				x, y := at[0]+dx, at[1]+dy
				if x >= 0 && y >= 0 && x < c.size && y < c.size {
					d := max(abs(dx), abs(dy))
					c.set(x, y, d != 2 && d != 4)
				}
			}
		}
	}
	pos := alignments(version, c.size)
	last := len(pos) - 1
	for i, ay := range pos {
		for j, ax := range pos {
			if (i == 0 && j == 0) || (i == 0 && j == last) || (i == last && j == 0) {
				continue // under a finder
			}
			for dy := -2; dy <= 2; dy++ {
				for dx := -2; dx <= 2; dx++ {
					c.set(ax+dx, ay+dy, max(abs(dx), abs(dy)) != 1)
				}
			}
		}
	}
	c.drawFormat(0) // reserves the format modules; the mask's bits come last
	if version >= 7 {
		rem := version
		for range 12 {
			rem = rem<<1 ^ (rem>>11)*0x1F25
		}
		bits := version<<12 | rem
		for i := range 18 {
			dark := bits>>i&1 == 1
			a, b := c.size-11+i%3, i/3
			c.set(a, b, dark)
			c.set(b, a, dark)
		}
	}
}

// alignments are the centers of a version's alignment patterns, on each
// axis.
func alignments(version, size int) []int {
	if version == 1 {
		return nil
	}
	n := version/7 + 2
	step := (version*8 + n*3 + 5) / (n*4 - 4) * 2
	pos := make([]int, n)
	pos[0] = 6
	for i, p := n-1, size-7; i >= 1; i, p = i-1, p-step {
		pos[i] = p
	}
	return pos
}

// drawFormat writes level M and the mask, with their BCH code, in both
// places, and the dark module.
func (c *Code) drawFormat(mask int) {
	data := 0<<3 | mask // level M is 00
	rem := data
	for range 10 {
		rem = rem<<1 ^ (rem>>9)*0x537
	}
	bits := (data<<10 | rem) ^ 0x5412
	bit := func(i int) bool { return bits>>i&1 == 1 }
	for i := range 6 {
		c.set(8, i, bit(i))
	}
	c.set(8, 7, bit(6))
	c.set(8, 8, bit(7))
	c.set(7, 8, bit(8))
	for i := 9; i < 15; i++ {
		c.set(14-i, 8, bit(i))
	}
	for i := range 8 {
		c.set(c.size-1-i, 8, bit(i))
	}
	for i := 8; i < 15; i++ {
		c.set(8, c.size-15+i, bit(i))
	}
	c.set(8, c.size-8, true)
}

// drawCodewords places the codewords' bits in the zigzag of two-module
// columns, right to left, skipping the vertical timing column.
func (c *Code) drawCodewords(data []byte) {
	i := 0
	for right := c.size - 1; right >= 1; right -= 2 {
		if right == 6 {
			right = 5
		}
		for vert := range c.size {
			for j := range 2 {
				x := right - j
				y := vert
				if (right+1)&2 == 0 {
					y = c.size - 1 - vert
				}
				if !c.function[y][x] && i < len(data)*8 {
					c.dark[y][x] = data[i>>3]>>(7-i&7)&1 == 1
					i++
				}
			}
		}
	}
}

func (c *Code) applyMask(mask int) {
	for y := range c.size {
		for x := range c.size {
			var flip bool
			switch mask {
			case 0:
				flip = (x+y)%2 == 0
			case 1:
				flip = y%2 == 0
			case 2:
				flip = x%3 == 0
			case 3:
				flip = (x+y)%3 == 0
			case 4:
				flip = (x/3+y/2)%2 == 0
			case 5:
				flip = x*y%2+x*y%3 == 0
			case 6:
				flip = (x*y%2+x*y%3)%2 == 0
			case 7:
				flip = ((x+y)%2+x*y%3)%2 == 0
			}
			if flip && !c.function[y][x] {
				c.dark[y][x] = !c.dark[y][x]
			}
		}
	}
}

// penalty scores a masked code by the standard's four rules: long runs,
// two-by-two blocks, finder-like patterns, and an unbalanced share of dark
// modules. The lowest score reads best.
func (c *Code) penalty() int {
	p := 0
	line := func(at func(i int) bool) {
		run := 1
		for i := 1; i <= c.size; i++ {
			if i < c.size && at(i) == at(i-1) {
				run++
				continue
			}
			if run >= 5 {
				p += 3 + run - 5
			}
			run = 1
		}
		finder := []bool{true, false, true, true, true, false, true}
		for i := 0; i+7 <= c.size; i++ {
			match := true
			for k, want := range finder {
				if at(i+k) != want {
					match = false
					break
				}
			}
			if !match {
				continue
			}
			light := func(from, to int) bool {
				for k := from; k < to; k++ {
					if k >= 0 && k < c.size && at(k) {
						return false
					}
				}
				return true
			}
			if light(i-4, i) || light(i+7, i+11) {
				p += 40
			}
		}
	}
	for y := range c.size {
		line(func(x int) bool { return c.dark[y][x] })
	}
	for x := range c.size {
		line(func(y int) bool { return c.dark[y][x] })
	}
	dark := 0
	for y := range c.size {
		for x := range c.size {
			if c.dark[y][x] {
				dark++
			}
			if x+1 < c.size && y+1 < c.size {
				d := c.dark[y][x]
				if c.dark[y][x+1] == d && c.dark[y+1][x] == d && c.dark[y+1][x+1] == d {
					p += 3
				}
			}
		}
	}
	total := c.size * c.size
	p += abs(dark*20-total*10) / total * 10
	return p
}

// rsDivisor is the Reed-Solomon generator polynomial of a degree, highest
// term first, its leading 1 left out.
func rsDivisor(degree int) []byte {
	out := make([]byte, degree)
	out[degree-1] = 1
	root := byte(1)
	for range degree {
		for j := range out {
			out[j] = gfMul(out[j], root)
			if j+1 < len(out) {
				out[j] ^= out[j+1]
			}
		}
		root = gfMul(root, 2)
	}
	return out
}

func rsRemainder(data, divisor []byte) []byte {
	out := make([]byte, len(divisor))
	for _, b := range data {
		factor := b ^ out[0]
		copy(out, out[1:])
		out[len(out)-1] = 0
		for i, d := range divisor {
			out[i] ^= gfMul(d, factor)
		}
	}
	return out
}

// gfMul multiplies in GF(2^8) modulo x^8 + x^4 + x^3 + x^2 + 1.
func gfMul(x, y byte) byte {
	z := 0
	for i := 7; i >= 0; i-- {
		z = z<<1 ^ (z>>7)*0x11D
		z ^= int(y>>i&1) * int(x)
	}
	return byte(z)
}

type bitBuffer []bool

func (b *bitBuffer) append(v, n int) {
	if n < 0 || v>>n != 0 && n < 31 {
		panic(errors.New("value does not fit its width"))
	}
	for i := n - 1; i >= 0; i-- {
		*b = append(*b, v>>i&1 == 1)
	}
}

func grid(n int) [][]bool {
	g := make([][]bool, n)
	for i := range g {
		g[i] = make([]bool, n)
	}
	return g
}

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}
