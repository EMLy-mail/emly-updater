//go:build windows

package tray

import "github.com/rodrigocfd/windigo/win"

// Fluent (Windows 11) look for the settings window: a navigation pane on the
// left, pages of rounded cards on the right, toggle switches, an InfoBar and
// an accent "Salva" button. Win32 has none of these, so they are drawn here:
//   - fluent.go          design tokens (this file);
//   - fluent_draw.go     antialiased primitives on a double-buffered canvas
//                        (GDI+ flat API for shapes, GDI for ClearType text),
//                        fonts, icon glyphs, card / row / input-frame painters;
//   - fluent_controls.go custom child controls (toggle, nav list, button,
//                        InfoBar, lock badge) built on ui.Control.
// Native Edit and ComboBox controls stay native, borderless, inside a frame
// painted by paintInputFrame.

// argb is a GDI+ colour, 0xAARRGGBB.
type argb uint32

func rgb(r, g, b uint8) argb { return 0xFF000000 | argb(r)<<16 | argb(g)<<8 | argb(b) }

// colorref converts to GDI's 0x00BBGGRR, dropping alpha.
func (c argb) colorref() win.COLORREF {
	return win.RGB(uint8(c>>16), uint8(c>>8), uint8(c))
}

// fluentPalette holds one theme's design tokens (WinUI 3 values, rounded).
type fluentPalette struct {
	windowBg, navBg                      argb // content area, navigation pane
	navHover, navSelected, navIndicator  argb // nav item states; indicator is the accent pill
	cardBg, cardStroke, divider          argb
	text, textSecondary, textDisabled    argb
	inputBg, inputStroke, inputBottom    argb // inputBottom: the darker bottom edge
	inputFocusBottom                     argb // accent bottom edge while focused
	accent, accentHover, accentPressed   argb
	textOnAccent                         argb
	buttonBg, buttonHover, buttonPressed argb
	buttonStroke                         argb
	infoBg, infoStroke, infoIcon         argb
	warnBg, warnStroke, warnIcon         argb
	badgeBg, badgeText                   argb
	toggleOffStroke, toggleOffKnob       argb // toggle off: outline and knob
	toggleOnKnob                         argb // knob on the accent track
	footerBg, footerStroke               argb
	focusStroke                          argb // keyboard focus rectangle
	popupBg, popupStroke                 argb // a fluentCombo's open list
	popupHover, popupSelected            argb // its items: under the mouse, chosen
}

var fluentDark = fluentPalette{
	windowBg: rgb(39, 39, 39), navBg: rgb(32, 32, 32),
	navHover: rgb(45, 45, 45), navSelected: rgb(50, 50, 50), navIndicator: rgb(96, 205, 255),
	cardBg: rgb(43, 43, 43), cardStroke: rgb(29, 29, 29), divider: rgb(29, 29, 29),
	text: rgb(255, 255, 255), textSecondary: rgb(197, 197, 197), textDisabled: rgb(120, 120, 120),
	inputBg: rgb(45, 45, 45), inputStroke: rgb(54, 54, 54), inputBottom: rgb(139, 139, 139),
	inputFocusBottom: rgb(96, 205, 255),
	accent:           rgb(96, 205, 255), accentHover: rgb(90, 186, 230), accentPressed: rgb(83, 168, 207),
	textOnAccent: rgb(0, 0, 0),
	buttonBg:     rgb(45, 45, 45), buttonHover: rgb(50, 50, 50), buttonPressed: rgb(39, 39, 39),
	buttonStroke: rgb(60, 60, 60),
	infoBg:       rgb(39, 50, 58), infoStroke: rgb(46, 66, 80), infoIcon: rgb(96, 205, 255),
	warnBg: rgb(67, 53, 25), warnStroke: rgb(88, 70, 30), warnIcon: rgb(252, 225, 0),
	badgeBg: rgb(50, 50, 50), badgeText: rgb(197, 197, 197),
	toggleOffStroke: rgb(205, 205, 205), toggleOffKnob: rgb(205, 205, 205), toggleOnKnob: rgb(0, 0, 0),
	footerBg: rgb(32, 32, 32), footerStroke: rgb(29, 29, 29),
	focusStroke: rgb(255, 255, 255),
	popupBg:     rgb(44, 44, 44), popupStroke: rgb(60, 60, 60),
	popupHover: rgb(61, 61, 61), popupSelected: rgb(54, 54, 54),
}

var fluentLight = fluentPalette{
	windowBg: rgb(249, 249, 249), navBg: rgb(243, 243, 243),
	navHover: rgb(234, 234, 234), navSelected: rgb(234, 234, 234), navIndicator: rgb(0, 95, 184),
	cardBg: rgb(255, 255, 255), cardStroke: rgb(229, 229, 229), divider: rgb(234, 234, 234),
	text: rgb(27, 27, 27), textSecondary: rgb(96, 96, 96), textDisabled: rgb(160, 160, 160),
	inputBg: rgb(255, 255, 255), inputStroke: rgb(229, 229, 229), inputBottom: rgb(134, 134, 134),
	inputFocusBottom: rgb(0, 95, 184),
	accent:           rgb(0, 95, 184), accentHover: rgb(25, 113, 194), accentPressed: rgb(49, 130, 204),
	textOnAccent: rgb(255, 255, 255),
	buttonBg:     rgb(251, 251, 251), buttonHover: rgb(246, 246, 246), buttonPressed: rgb(245, 245, 245),
	buttonStroke: rgb(214, 214, 214),
	infoBg:       rgb(244, 248, 252), infoStroke: rgb(208, 226, 244), infoIcon: rgb(0, 95, 184),
	warnBg: rgb(255, 244, 206), warnStroke: rgb(240, 220, 160), warnIcon: rgb(157, 93, 0),
	badgeBg: rgb(240, 240, 240), badgeText: rgb(96, 96, 96),
	toggleOffStroke: rgb(134, 134, 134), toggleOffKnob: rgb(93, 93, 93), toggleOnKnob: rgb(255, 255, 255),
	footerBg: rgb(243, 243, 243), footerStroke: rgb(229, 229, 229),
	focusStroke: rgb(0, 0, 0),
	popupBg:     rgb(249, 249, 249), popupStroke: rgb(229, 229, 229),
	popupHover: rgb(236, 236, 236), popupSelected: rgb(241, 241, 241),
}

// fp returns the palette of the theme in use (isDark, themeMode). UI thread only.
func fp() *fluentPalette {
	if isDark() {
		return &fluentDark
	}
	return &fluentLight
}

// Icon glyphs: Segoe Fluent Icons (Windows 11), with the same code points
// in Segoe MDL2 Assets (Windows 10), see iconFontFace.
const (
	glyphGlobe       = '' // Connessione
	glyphSync        = '' // Aggiornamenti
	glyphServer      = '' // Servizio: "Processing" gears; neither icon font has a stacked-server glyph
	glyphPackage     = '' // Prodotti
	glyphColor       = '' // Aspetto
	glyphInfo        = ''
	glyphWarning     = ''
	glyphLock        = ''
	glyphShield      = '' // on "Salva": it asks for elevation
	glyphChevronDown = ''
)

// Layout metrics, in 96-DPI units (scale with ui.DpiX/DpiY).
const (
	cardRadius    = 6 // cards, InfoBar
	controlRadius = 4 // buttons, inputs, nav items, badges
	rowMinHeight  = 66
	rowPadX       = 16
)
