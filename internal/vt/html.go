package vt

import (
	"fmt"
	"strings"
)

// RenderRowHTML returns a row as HTML spans with inline styles/classes for colors.
func (s *Screen) RenderRowHTML(y int) string {
	if y < 0 || y >= s.Height {
		return ""
	}
	var buf strings.Builder
	for _, cell := range s.Cells[y] {
		cls, style := styleToCSS(cell.Style)
		buf.WriteString(`<span class="cell`)
		if cls != "" {
			buf.WriteByte(' ')
			buf.WriteString(cls)
		}
		buf.WriteByte('"')
		if style != "" {
			buf.WriteString(` style="`)
			buf.WriteString(style)
			buf.WriteByte('"')
		}
		buf.WriteByte('>')
		writeEscapedRune(&buf, cell.Ch)
		buf.WriteString("</span>")
	}
	return buf.String()
}

func writeEscapedRune(buf *strings.Builder, r rune) {
	switch r {
	case '<':
		buf.WriteString("&lt;")
	case '>':
		buf.WriteString("&gt;")
	case '&':
		buf.WriteString("&amp;")
	case '"':
		buf.WriteString("&quot;")
	default:
		buf.WriteRune(r)
	}
}

func styleToCSS(st Style) (classes, inline string) {
	if st == (Style{}) {
		return "", ""
	}
	var cls []string
	var styles []string

	if st.Bold {
		styles = append(styles, "font-weight:bold")
	}
	if st.Dim {
		styles = append(styles, "opacity:.5")
	}
	if st.Italic {
		styles = append(styles, "font-style:italic")
	}
	if st.Underline {
		styles = append(styles, "text-decoration:underline")
	}
	if st.Strikethrough {
		styles = append(styles, "text-decoration:line-through")
	}
	if st.Underline && st.Strikethrough {
		styles = styles[:len(styles)-2]
		styles = append(styles, "text-decoration:underline line-through")
	}

	fg := st.FG
	bg := st.BG
	if st.Inverse {
		fg, bg = bg, fg
		// If either was default, swap to the other's "default" visual
		if fg.Type == 0 {
			styles = append(styles, "color:var(--bg)")
		}
		if bg.Type == 0 {
			styles = append(styles, "background:var(--text)")
		}
	}

	cls, styles = appendColorCSS(cls, styles, fg, "fg")
	cls, styles = appendColorCSS(cls, styles, bg, "bg")

	return strings.Join(cls, " "), strings.Join(styles, ";")
}

func appendColorCSS(cls, styles []string, c Color, prefix string) (outCls, outStyles []string) {
	prop := "color"
	if prefix == "bg" {
		prop = "background"
	}
	switch c.Type {
	case 1: // basic 8/16 color — use CSS class for theming
		cls = append(cls, fmt.Sprintf("term-%s-%d", prefix, c.Val))
	case 2: // 256-color
		styles = append(styles, fmt.Sprintf("%s:%s", prop, color256ToHex(c.Val)))
	case 3: // RGB
		styles = append(styles, fmt.Sprintf("%s:#%02x%02x%02x", prop, c.R, c.G, c.B))
	}
	return cls, styles
}

func color256ToHex(idx uint8) string {
	// Standard 16 colors
	if idx < 16 {
		return [16]string{
			"#000", "#a00", "#0a0", "#a50", "#00a", "#a0a", "#0aa", "#aaa",
			"#555", "#f55", "#5f5", "#ff5", "#55f", "#f5f", "#5ff", "#fff",
		}[idx]
	}
	// 216-color cube (indices 16-231)
	if idx < 232 {
		i := idx - 16
		b := i % 6
		g := (i / 6) % 6
		r := i / 36
		toVal := func(v uint8) uint8 {
			if v == 0 {
				return 0
			}
			return 55 + v*40
		}
		return fmt.Sprintf("#%02x%02x%02x", toVal(r), toVal(g), toVal(b))
	}
	// Grayscale (indices 232-255)
	v := 8 + (idx-232)*10
	return fmt.Sprintf("#%02x%02x%02x", v, v, v)
}
