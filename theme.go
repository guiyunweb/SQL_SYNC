package main

import (
	"image/color"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/theme"
)

// sqlsyncTheme 在 Fyne 默认主题上做品牌化定制：
// 主色改为科技蓝、成功/危险色更鲜明、正文稍大更易读。
type sqlsyncTheme struct{ base fyne.Theme }

// brandPrimary 品牌主色，界面头部横幅等处也直接引用。
var brandPrimary = color.NRGBA{R: 0x25, G: 0x63, B: 0xEB, A: 0xFF} // #2563EB

// brandAccent 头部渐变的终点色（靛蓝）。
var brandAccent = color.NRGBA{R: 0x4F, G: 0x46, B: 0xE5, A: 0xFF} // #4F46E5

func newTheme() *sqlsyncTheme { return &sqlsyncTheme{base: theme.DefaultTheme()} }

func (t *sqlsyncTheme) Color(name fyne.ThemeColorName, v fyne.ThemeVariant) color.Color {
	switch name {
	case theme.ColorNamePrimary, theme.ColorNameHyperlink:
		return brandPrimary
	case theme.ColorNameFocus:
		return color.RGBA{R: 0x25, G: 0x63, B: 0xEB, A: 0x66}
	case theme.ColorNameSuccess:
		return color.RGBA{R: 0x0D, G: 0xB5, B: 0x7E, A: 0xFF} // #0DB57E
	case theme.ColorNameError:
		return color.RGBA{R: 0xEF, G: 0x44, B: 0x44, A: 0xFF} // #EF4444
	}
	// 次要文字（说明、占位符、耗时）：默认值在两种模式下对比度都偏低
	if name == theme.ColorNamePlaceHolder || name == theme.ColorNameDisabled {
		if v == theme.VariantLight {
			return color.RGBA{R: 0x80, G: 0x8A, B: 0x9B, A: 0xFF} // #808A9B
		}
		return color.RGBA{R: 0x8E, G: 0x97, B: 0xA6, A: 0xFF} // #8E97A6
	}
	// 浅色模式：窗口底色略灰，卡片纯白，形成层次感
	if v == theme.VariantLight {
		switch name {
		case theme.ColorNameBackground:
			return color.RGBA{R: 0xF3, G: 0xF5, B: 0xF9, A: 0xFF} // #F3F5F9
		case theme.ColorNameOverlayBackground:
			return color.White
		}
	}
	return t.base.Color(name, v)
}

func (t *sqlsyncTheme) Font(s fyne.TextStyle) fyne.Resource     { return t.base.Font(s) }
func (t *sqlsyncTheme) Icon(n fyne.ThemeIconName) fyne.Resource { return t.base.Icon(n) }

func (t *sqlsyncTheme) Size(name fyne.ThemeSizeName) float32 {
	switch name {
	case theme.SizeNameText:
		return 14
	case theme.SizeNameHeadingText:
		return 19
	case theme.SizeNamePadding:
		return 6
	case theme.SizeNameInputRadius, theme.SizeNameSelectionRadius:
		return 6
	}
	return t.base.Size(name)
}
