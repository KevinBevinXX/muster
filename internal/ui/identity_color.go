package ui

import (
	"fmt"
	"math"
	"strconv"
	"sync"

	g "maragu.dev/gomponents"

	"github.com/ZacxDev/muster/internal/mirror"
)

// THE TMUX IDENTITY COLOUR on a mirrored card (docs/design/agent-cards.md
// (b)): the colour the host's tmux config gives the window (`@muster-color`),
// on the card's BORDER and its TITLE, so the card and the terminal look like
// the same thing.
//
// 🔴 READABLE IN BOTH THEMES, BY MOVING ONLY LIGHTNESS. A terminal colour is
// chosen for a terminal's background, not for this card's. Each variant keeps
// the colour's OKLCH hue and chroma and moves its lightness, toward the side
// that gains contrast, just far enough to meet:
//
//   - 3:1 for the border (WCAG 1.4.11: a component's boundary);
//   - 4.5:1 for the title (WCAG 1.4.3: text);
//
// against BOTH of the theme's surfaces a card can sit over — `s1` and `bg`
// (the card is s1 at 70% over bg, so it lies between them, and contrast is
// monotone in luminance). In the light theme pale colours (cream, light
// yellow) visibly darken: the operator accepted that as the price of a
// readable title (operator decision 2).
//
// 🔴 NEVER HOST TEXT IN THE STYLE ATTRIBUTE. The colour reaching this file is
// already mirror.ValidColor's lowercase #rrggbb (both halves of the wire
// normalise it); it is parsed here into integers, and the attribute is built
// from those integers alone. Anything else draws no tint at all.
//
// 🔴 THE COLOUR IS NEVER THE ONLY IDENTIFIER (WCAG 1.4.1): the title, label
// and host text are unchanged, which is also why two slots sharing a colour
// is harmless. It does not touch the recency dot or the status glyph:
// recency is the dot's, status the glyph's and its text's.

// idRGB is an sRGB colour, 0-255 per channel.
type idRGB struct{ r, g, b int }

// The surfaces a tinted card sits over, per theme — web/css/input.css's `s1`
// and `bg` tokens, as integers (TestIdentitySurfacesAreTheThemeTokens holds
// them to the stylesheet, so they cannot drift).
var (
	identitySurfacesDark  = []idRGB{{25, 22, 42}, {17, 15, 28}}
	identitySurfacesLight = []idRGB{{255, 255, 255}, {246, 244, 251}}
)

// The contrast each variant must meet against every surface of its theme.
const (
	identityBorderContrast = 3.0
	identityTitleContrast  = 4.5
)

// IdentityTint is one colour's four variants: border and title, dark and
// light theme.
type IdentityTint struct {
	BorderDark, BorderLight, TitleDark, TitleLight idRGB
}

// identityCache memoises identityTint per colour. 🔴 BOUNDED: the colours are
// values hosts send, so a host cycling through distinct ones must not grow it
// without limit. Past identityCacheMax entries it is cleared and refilled;
// an operator's palette (a few dozen slot colours) stays resident.
var identityCache = struct {
	sync.Mutex
	m    map[string]IdentityTint
	hits int
}{m: map[string]IdentityTint{}}

// identityCacheMax bounds identityCache.
const identityCacheMax = 2048

// identityTint is c's variants, and false when c is not a wire colour.
func identityTint(c string) (IdentityTint, bool) {
	if !mirror.ValidColor(c) {
		return IdentityTint{}, false
	}
	identityCache.Lock()
	if v, ok := identityCache.m[c]; ok {
		identityCache.hits++
		identityCache.Unlock()
		return v, true
	}
	identityCache.Unlock()
	n, err := strconv.ParseUint(c[1:], 16, 32)
	if err != nil {
		return IdentityTint{}, false
	}
	base := idRGB{int(n >> 16 & 0xff), int(n >> 8 & 0xff), int(n & 0xff)}
	t := IdentityTint{
		BorderDark:  meetContrast(base, identitySurfacesDark, identityBorderContrast, true),
		BorderLight: meetContrast(base, identitySurfacesLight, identityBorderContrast, false),
		TitleDark:   meetContrast(base, identitySurfacesDark, identityTitleContrast, true),
		TitleLight:  meetContrast(base, identitySurfacesLight, identityTitleContrast, false),
	}
	identityCache.Lock()
	if len(identityCache.m) >= identityCacheMax {
		clear(identityCache.m)
	}
	identityCache.m[c] = t
	identityCache.Unlock()
	return t, true
}

// identityAttrs is what a tinted element carries: data-identity, and its four
// variants as CSS custom properties ("R G B", the palette tokens' own form),
// which web/css/input.css maps to the theme in force — no script involved. No
// colour (or an invalid one) is no attributes: the element looks exactly as
// it did before tints existed.
func identityAttrs(c string) g.Node {
	t, ok := identityTint(c)
	if !ok {
		return g.Group(nil)
	}
	v := func(x idRGB) string { return fmt.Sprintf("%d %d %d", x.r, x.g, x.b) }
	return g.Group([]g.Node{
		g.Attr("data-identity", c),
		g.Attr("style", "--id-bd-d:"+v(t.BorderDark)+";--id-bd-l:"+v(t.BorderLight)+";--id-tt-d:"+v(t.TitleDark)+";--id-tt-l:"+v(t.TitleLight)),
	})
}

// meetContrast is c with its OKLCH lightness moved — up when lighten, down
// otherwise — by the smallest step that meets min against every surface; c
// itself when it already does. White and black meet every target here, so the
// walk always ends.
func meetContrast(c idRGB, surfaces []idRGB, min float64, lighten bool) idRGB {
	if minContrast(c, surfaces) >= min {
		return c
	}
	l, a, b := toOKLab(c)
	chroma, hue := math.Hypot(a, b), math.Atan2(b, a)
	step := -0.002
	if lighten {
		step = 0.002
	}
	for i := 1; i <= 500; i++ {
		nl := l + step*float64(i)
		if nl > 1 || nl < 0 {
			break
		}
		x := fromOKLCH(nl, chroma, hue)
		if minContrast(x, surfaces) >= min {
			return x
		}
	}
	if lighten {
		return idRGB{255, 255, 255}
	}
	return idRGB{0, 0, 0}
}

// minContrast is c's lowest WCAG contrast ratio against surfaces.
func minContrast(c idRGB, surfaces []idRGB) float64 {
	low := math.Inf(1)
	for _, s := range surfaces {
		low = math.Min(low, idContrast(c, s))
	}
	return low
}

// contrastRatio is WCAG 2's contrast ratio of a and b.
func idContrast(a, b idRGB) float64 {
	la, lb := idLuminance(a), idLuminance(b)
	if la < lb {
		la, lb = lb, la
	}
	return (la + 0.05) / (lb + 0.05)
}

func idLuminance(c idRGB) float64 {
	return 0.2126*idToLinear(c.r) + 0.7152*idToLinear(c.g) + 0.0722*idToLinear(c.b)
}

func idToLinear(v int) float64 {
	x := float64(v) / 255
	if x <= 0.04045 {
		return x / 12.92
	}
	return math.Pow((x+0.055)/1.055, 2.4)
}

func idFromLinear(x float64) int {
	x = math.Max(0, math.Min(1, x))
	if x <= 0.0031308 {
		x *= 12.92
	} else {
		x = 1.055*math.Pow(x, 1/2.4) - 0.055
	}
	return int(math.Round(x * 255))
}

// toOKLab and oklabLinear are Björn Ottosson's OKLab transforms (linear sRGB).
func toOKLab(c idRGB) (l, a, b float64) {
	r, gr, bl := idToLinear(c.r), idToLinear(c.g), idToLinear(c.b)
	lc := math.Cbrt(0.4122214708*r + 0.5363325363*gr + 0.0514459929*bl)
	mc := math.Cbrt(0.2119034982*r + 0.6806995451*gr + 0.1073969566*bl)
	sc := math.Cbrt(0.0883024619*r + 0.2817188376*gr + 0.6299787005*bl)
	return 0.2104542553*lc + 0.7936177850*mc - 0.0040720468*sc,
		1.9779984951*lc - 2.4285922050*mc + 0.4505937099*sc,
		0.0259040371*lc + 0.7827717662*mc - 0.8086757660*sc
}

// oklabLinear is OKLab (l, a, b) as linear sRGB, unclipped.
func oklabLinear(l, a, b float64) (r, g, bl float64) {
	lc := l + 0.3963377774*a + 0.2158037573*b
	mc := l - 0.1055613458*a - 0.0638541728*b
	sc := l - 0.0894841775*a - 1.2914855480*b
	lc, mc, sc = lc*lc*lc, mc*mc*mc, sc*sc*sc
	return 4.0767416621*lc - 3.3077115913*mc + 0.2309699292*sc,
		-1.2684380046*lc + 2.6097574011*mc - 0.3413193965*sc,
		-0.0041960863*lc - 0.7034186147*mc + 1.7076147010*sc
}

// fromOKLCH is the sRGB colour of OKLCH (l, chroma, hue). One outside the sRGB
// gamut has its CHROMA reduced (bisected) until it fits, so its hue and
// lightness are kept — clipping each channel instead would shift the hue.
func fromOKLCH(l, chroma, hue float64) idRGB {
	in := func(c float64) bool {
		r, g, b := oklabLinear(l, c*math.Cos(hue), c*math.Sin(hue))
		const eps = 1e-4
		return r >= -eps && r <= 1+eps && g >= -eps && g <= 1+eps && b >= -eps && b <= 1+eps
	}
	if !in(chroma) {
		lo, hi := 0.0, chroma
		for i := 0; i < 24; i++ {
			mid := (lo + hi) / 2
			if in(mid) {
				lo = mid
			} else {
				hi = mid
			}
		}
		chroma = lo
	}
	r, g, b := oklabLinear(l, chroma*math.Cos(hue), chroma*math.Sin(hue))
	return idRGB{idFromLinear(r), idFromLinear(g), idFromLinear(b)}
}
