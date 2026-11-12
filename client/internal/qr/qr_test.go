package qr

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// testdata/qrcode.json holds codes the Python qrcode library made of the same texts
// at level M with a given mask: versions 1 to 10, every mask.
func TestCodesMatchAnotherEncoder(t *testing.T) {
	b, err := os.ReadFile("testdata/qrcode.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		Text    string   `json:"text"`
		Version int      `json:"version"`
		Mask    int      `json:"mask"`
		Rows    []string `json:"rows"`
	}
	if err := json.Unmarshal(b, &cases); err != nil {
		t.Fatal(err)
	}
	for _, tc := range cases {
		c, err := encode([]byte(tc.Text), tc.Mask)
		if err != nil {
			t.Fatalf("%d bytes: %v", len(tc.Text), err)
		}
		if got := (c.Size() - 17) / 4; got != tc.Version {
			t.Fatalf("%d bytes: version %d, want %d", len(tc.Text), got, tc.Version)
		}
		for y, row := range tc.Rows {
			var sb strings.Builder
			for x := range c.Size() {
				if c.Dark(x, y) {
					sb.WriteByte('1')
				} else {
					sb.WriteByte('0')
				}
			}
			if sb.String() != row {
				t.Fatalf("version %d mask %d: row %d\n%s\nwant\n%s", tc.Version, tc.Mask, y, sb.String(), row)
			}
		}
	}
}

func TestDrawings(t *testing.T) {
	c, err := Encode("https://box.example.ts.net/#pair=ABCD-EFGH-JKLM")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(c.Terminal(), "\n"), "\n")
	if want := (c.Size() + 2*quiet + 1) / 2; len(lines) != want {
		t.Fatalf("%d lines, want %d", len(lines), want)
	}
	svg := c.SVG()
	if !strings.HasPrefix(svg, "<svg ") || !strings.Contains(svg, `viewBox="0 0 41 41"`) {
		t.Fatalf("svg %.80s", svg)
	}
	if _, err := Encode(strings.Repeat("x", 214)); err == nil {
		t.Fatal("214 bytes encoded")
	}
}
