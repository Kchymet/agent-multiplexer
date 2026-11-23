package main

import (
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"os"
	"strings"

	"github.com/boombuler/barcode/qr"
)

// QR output for `amux direct enroll --qr / --qr-png`. The encoded text is an
// enrollment secret: nothing here logs it, and errors never include it.

// qrQuietZone is the light border around the symbol, in modules (ISO/IEC
// 18004 requires at least 4).
const qrQuietZone = 4

// qrPNGScale is the PNG's pixels per module.
const qrPNGScale = 8

// qrMatrix encodes text at error-correction level M in byte mode and returns
// its modules (true = dark), without the quiet zone. Byte mode is chosen
// explicitly: the encoder's automatic mode quotes the content in its error,
// and a ticket is mixed-case base64url, which only byte mode can hold anyway.
func qrMatrix(text string) ([][]bool, error) {
	code, err := qr.Encode(text, qr.M, qr.Unicode)
	if err != nil {
		// The library's byte-mode error is content-free ("too much data").
		return nil, fmt.Errorf("encode QR code (%d bytes): %w", len(text), err)
	}
	n := code.Bounds().Dx()
	m := make([][]bool, n)
	for y := range m {
		m[y] = make([]bool, n)
		for x := range m[y] {
			m[y][x] = color.GrayModel.Convert(code.At(x, y)).(color.Gray).Y < 128
		}
	}
	return m, nil
}

// qrVersion is the QR version of a symbol of n modules per side.
func qrVersion(n int) int { return (n - 17) / 4 }

// withQuietZone pads m with qrQuietZone light modules on every side.
func withQuietZone(m [][]bool) [][]bool {
	n := len(m) + 2*qrQuietZone
	out := make([][]bool, n)
	for y := range out {
		out[y] = make([]bool, n)
		if y >= qrQuietZone && y < n-qrQuietZone {
			copy(out[y][qrQuietZone:], m[y-qrQuietZone])
		}
	}
	return out
}

// Fixed xterm-256 palette entries: 16 is pure black and 231 pure white in the
// color cube, which terminal themes do not remap (unlike the 16 ANSI colors).
const (
	sgrDarkOnLight = "\x1b[38;5;16;48;5;231m"
	sgrLightOnDark = "\x1b[38;5;231;48;5;16m"
	sgrReset       = "\x1b[0m"
)

// renderQRHalfBlocks draws text as a QR code with Unicode half blocks: each
// character cell holds two modules stacked vertically, so the symbol keeps
// its square shape in a terminal.
//
// With colors (the default) every line pins black modules on a white
// background, so the code scans the same on dark and light terminal themes.
// Without colors (NO_COLOR, TERM=dumb) the terminal's own colors draw it:
// filled glyphs are dark modules, right for a light-background terminal.
// invert makes the filled glyphs the light modules instead (right for a
// dark-background terminal without colors); with colors the result looks the
// same either way, so invert only matters where colors are not honored.
func renderQRHalfBlocks(w io.Writer, text string, invert, colors bool) error {
	m, err := qrMatrix(text)
	if err != nil {
		return err
	}
	q := withQuietZone(m)
	if len(q)%2 == 1 {
		// An odd final row is paired with one more light row.
		q = append(q, make([]bool, len(q)))
	}
	filled := func(dark bool) bool { return dark != invert }
	var b strings.Builder
	for y := 0; y < len(q); y += 2 {
		if colors {
			if invert {
				b.WriteString(sgrLightOnDark)
			} else {
				b.WriteString(sgrDarkOnLight)
			}
		}
		for x := range q[y] {
			top, bottom := filled(q[y][x]), filled(q[y+1][x])
			switch {
			case top && bottom:
				b.WriteString("█")
			case top:
				b.WriteString("▀")
			case bottom:
				b.WriteString("▄")
			default:
				b.WriteByte(' ')
			}
		}
		if colors {
			b.WriteString(sgrReset)
		}
		b.WriteByte('\n')
	}
	_, err = io.WriteString(w, b.String())
	return err
}

// terminalColors reports whether the QR may pin its own colors.
func terminalColors(getenv func(string) string) bool {
	return getenv("NO_COLOR") == "" && getenv("TERM") != "dumb"
}

// createQRPNG creates path exclusively with mode 0600. It never opens,
// truncates or re-permissions an existing file: a name that already exists
// (including a symlink) is refused.
func createQRPNG(path string) (*os.File, error) {
	if path == "" {
		return nil, errors.New("--qr-png needs a file path")
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return nil, fmt.Errorf("--qr-png %s already exists; choose a new path (amux never overwrites it)", path)
		}
		return nil, fmt.Errorf("--qr-png: %w", err)
	}
	return f, nil
}

// writeQRPNG writes text as a black-on-white PNG QR code (with its quiet
// zone) to f, then syncs and closes f.
func writeQRPNG(f *os.File, text string) error {
	m, err := qrMatrix(text)
	if err != nil {
		f.Close()
		return err
	}
	q := withQuietZone(m)
	n := len(q) * qrPNGScale
	img := image.NewGray(image.Rect(0, 0, n, n))
	for py := 0; py < n; py++ {
		for px := 0; px < n; px++ {
			v := uint8(255)
			if q[py/qrPNGScale][px/qrPNGScale] {
				v = 0
			}
			img.Pix[py*img.Stride+px] = v
		}
	}
	if err := png.Encode(f, img); err != nil {
		f.Close()
		return fmt.Errorf("write QR PNG: %w", err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return fmt.Errorf("write QR PNG: %w", err)
	}
	return f.Close()
}
