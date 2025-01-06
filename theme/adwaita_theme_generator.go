//go:build ignore
// +build ignore

/*
This tool generates the Adwaita theme for Fyne.

It takes the colors from the Adwaita page: https://gnome.pages.gitlab.gnome.org/libadwaita/doc/1.0/named-colors.html
and the icons from the Adwaita icon theme: https://gitlab.gnome.org/GNOME/adwaita-icon-theme

There are 2 outputs:
- adwaita_colors.go: the colors for the theme as map[fyne.ThemeColorName]color.Color
- adwaita_icons.go: the icons for the theme as fyne.Resource (themed for symbolic icons)

Usage:
go generate ./theme/...
*/

package main

import (
	"archive/tar"
	"bytes"
	"fmt"
	"go/format"
	"image/color"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"text/template"

	"fyne.io/fyne/v2"
)

const (
	adwaitaColorPage  = "https://gnome.pages.gitlab.gnome.org/libadwaita/doc/1.0/named-colors.html"                                     // the gnome page describing the colors
	adwaitaIconsPage  = "https://gitlab.gnome.org/GNOME/adwaita-icon-theme/-/archive/master/adwaita-icon-theme-master.tar?path=Adwaita" // gitlab page with the icons, tar file here
	colorSchemeOutput = "adwaita_colors.go"
	iconsOutput       = "adwaita_icons.go"

	// the template to generate the color scheme
	colorSourceTpl = `package theme

// This file is generated by adwaita_theme_generator.go
// Please do not edit manually, use:
// go generate ./theme/...
//
// The colors are taken from: https://gnome.pages.gitlab.gnome.org/libadwaita/doc/1.0/named-colors.html

import (
	"image/color"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/theme"
)

var adwaitaDarkScheme = map[fyne.ThemeColorName]color.Color{
{{- range $key, $value := .DarkScheme }}
    {{ $key }}: {{ printf "color.NRGBA{R: 0x%02x, G: 0x%02x, B: 0x%02x, A: 0x%02x}" $value.Col.R $value.Col.G $value.Col.B $value.Col.A }}, // Adwaita color name @{{$value.AdwName}}
{{- end }}
}

var adwaitaLightScheme = map[fyne.ThemeColorName]color.Color{
{{- range $key, $value := .LightScheme }}
    {{ $key }}: {{ printf "color.NRGBA{R: 0x%02x, G: 0x%02X, B: 0x%02x, A: 0x%02x}" $value.Col.R $value.Col.G $value.Col.B $value.Col.A }}, // Adwaita color name @{{$value.AdwName}}
{{- end }}
}`
	// the template where to bundle the icons in a map
	iconSourceTpl = `package theme

// This file is generated by adwaita_theme_generator.go
// Please do not edit manually, use:
// go generate ./theme/...
//
// This icons come from "GNOME Project"
// Repository: https://gitlab.gnome.org/GNOME/adwaita-icon-theme
// Licence: CC-BY-SA 3.0
// See: https://gitlab.gnome.org/GNOME/adwaita-icon-theme/-/blob/master/COPYING_CCBYSA3

import (
    "fyne.io/fyne/v2"
    "fyne.io/fyne/v2/theme"
)

var adwaitaIcons = map[fyne.ThemeIconName]fyne.Resource{
{{range $name, $icon := .Icons}}
{{ if contains $icon.StaticName "symbolic" }}
    theme.{{$name}}: theme.NewThemedResource(&fyne.StaticResource{
        StaticName: "{{$icon.StaticName}}",
        StaticContent: []byte({{ printf "%q" $icon.Content}}),
    }),
{{else}}
    theme.{{$name}}: &fyne.StaticResource{
        StaticName: "{{$icon.StaticName}}",
        StaticContent: []byte({{ printf "%q" $icon.Content}}),
    },
{{end}}
{{end}}
}
`
)

var (
	// All colors are described in a table. Each color is a row.
	tableRowMatcher = regexp.MustCompile(`(?s)<tr>(.*?)</tr>`)

	// The color values are in a <tt> tag, the first one is the light color, the second one is the dark color.
	// The color is described in a rgba() format, or in a #RRGGBB format.
	tableColorCellMatcher = regexp.MustCompile(`(?s)<tt>((?:rgba|#).*?)</tt>`)

	// map to describe the colors to get from the Adwaita page and the name of the color in the Fyne theme
	colorToGet = map[string]string{
		"theme.ColorNameBackground":        "window_bg_color",  // or "view_bg_color"
		"theme.ColorNameForeground":        "window_fg_color",  // or "view_fg_color"
		"theme.ColorNameMenuBackground":    "popover_bg_color", // not sure about this one
		"theme.ColorNameSelection":         "headerbar_bg_color",
		"theme.ColorNameOverlayBackground": "view_bg_color",      // not sure about this one
		"theme.ColorNamePrimary":           "accent_bg_color",    // accent_color is the primary color for Adwaita
		"theme.ColorNameInputBackground":   "view_bg_color",      // or "window_bg_color"
		"theme.ColorNameButton":            "headerbar_bg_color", // it's the closer color to the button color
		"theme.ColorNameShadow":            "shade_color",
		"theme.ColorNameSuccess":           "success_bg_color",
		"theme.ColorNameWarning":           "warning_bg_color", // Adwaita doesn't have "orange_x" color for "dark"
		"theme.ColorNameError":             "error_bg_color",
	}

	// and standard color names:
	// if the key has got 2 values, the first one is the light color, the second one is the dark color
	standardColorToGet = map[string]string{
		"theme.ColorRed":    "red_3,red_4",     // based on error_bg_color
		"theme.ColorOrange": "orange_3",        // more or less the same as warning_bg_color
		"theme.ColorYellow": "yellow_3",        // more or less the same as warning_bg_color
		"theme.ColorGreen":  "green_4,green_5", // based on success_bg_color
		"theme.ColorBlue":   "blue_3",
		"theme.ColorPurple": "purple_3",
		"theme.ColorBrown":  "brown_3",
		"theme.ColorGray":   "dark_2",
		// specific
		"theme.ColorNameScrollBar": "dark_5,light_1", // and we will change the alpha value later
	}

	// map to describe the icons to get from the Adwaita gitlab page and the name of the icon in the Fyne theme
	// note: all empty icons are those that are not defined yet. We should make some choices about them.
	iconsToGet = map[string]string{
		"IconNameCancel":        "symbolic/ui/window-close-symbolic.svg",
		"IconNameConfirm":       "symbolic/actions/object-select-symbolic.svg",
		"IconNameDelete":        "symbolic/actions/edit-delete-symbolic.svg",
		"IconNameSearch":        "symbolic/actions/edit-find-symbolic.svg",
		"IconNameSearchReplace": "symbolic/actions/edit-find-replace-symbolic.svg",
		"IconNameMenu":          "symbolic/actions/open-menu-symbolic.svg",
		"IconNameMenuExpand":    "symbolic/ui/pan-end-symbolic.svg",

		"IconNameCheckButton":        "symbolic/ui/checkbox-symbolic.svg",
		"IconNameCheckButtonChecked": "symbolic/ui/checkbox-checked-symbolic.svg",
		"IconNameRadioButton":        "symbolic/ui/radio-symbolic.svg",
		"IconNameRadioButtonChecked": "symbolic/ui/radio-checked-symbolic.svg",

		"IconNameContentAdd":    "symbolic/actions/list-add-symbolic.svg",
		"IconNameContentClear":  "symbolic/actions/edit-clear-symbolic.svg",
		"IconNameContentRemove": "symbolic/actions/list-remove-symbolic.svg",
		"IconNameContentCut":    "symbolic/actions/edit-cut-symbolic.svg",
		"IconNameContentCopy":   "symbolic/actions/edit-copy-symbolic.svg",
		"IconNameContentPaste":  "symbolic/actions/edit-paste-symbolic.svg",
		"IconNameContentRedo":   "symbolic/actions/edit-redo-symbolic.svg",
		"IconNameContentUndo":   "symbolic/actions/edit-undo-symbolic.svg",

		"IconNameColorAchromatic": "",
		"IconNameColorChromatic":  "",
		"IconNameColorPalette":    "symbolic/categories/applications-graphics-symbolic.svg",

		"IconNameDocument":       "symbolic/mimetypes/text-x-generic-symbolic.svg",
		"IconNameDocumentCreate": "symbolic/actions/document-new-symbolic.svg",
		"IconNameDocumentPrint":  "symbolic/actions/document-print-symbolic.svg",
		"IconNameDocumentSave":   "symbolic/actions/document-save-symbolic.svg",

		"IconNameMoreHorizontal": "symbolic/actions/view-more-horizontal-symbolic.svg",
		"IconNameMoreVertical":   "symbolic/actions/view-more-symbolic.svg",

		"IconNameInfo":     "symbolic/status/dialog-information-symbolic.svg",
		"IconNameQuestion": "symbolic/status/dialog-question-symbolic.svg",
		"IconNameWarning":  "symbolic/status/dialog-warning-symbolic.svg",
		"IconNameError":    "symbolic/status/dialog-error-symbolic.svg",

		"IconNameMailAttachment": "symbolic/status/mail-attachment-symbolic.svg",
		"IconNameMailCompose":    "symbolic/actions/mail-message-new-symbolic.svg",
		"IconNameMailForward":    "symbolic/actions/mail-forward-symbolic.svg",
		"IconNameMailReply":      "symbolic/actions/mail-reply-sender-symbolic.svg",
		"IconNameMailReplyAll":   "symbolic/actions/mail-reply-all-symbolic.svg",
		"IconNameMailSend":       "symbolic/actions/mail-send-symbolic.svg",

		"IconNameMediaMusic":        "symbolic/mimetypes/audio-x-generic-symbolic.svg",
		"IconNameMediaPhoto":        "symbolic/mimetypes/image-x-generic-symbolic.svg",
		"IconNameMediaVideo":        "symbolic/mimetypes/video-x-generic-symbolic.svg",
		"IconNameMediaFastForward":  "symbolic/actions/media-seek-forward-symbolic.svg",
		"IconNameMediaFastRewind":   "symbolic/actions/media-seek-backward-symbolic.svg",
		"IconNameMediaPause":        "symbolic/actions/media-playback-pause-symbolic.svg",
		"IconNameMediaPlay":         "symbolic/actions/media-playback-start-symbolic.svg",
		"IconNameMediaRecord":       "symbolic/actions/media-record-symbolic.svg",
		"IconNameMediaReplay":       "symbolic/actions/media-seek-backward-symbolic.svg",
		"IconNameMediaSkipNext":     "symbolic/actions/media-skip-forward-symbolic.svg",
		"IconNameMediaSkipPrevious": "symbolic/actions/media-skip-backward-symbolic.svg",
		"IconNameMediaStop":         "symbolic/actions/media-playback-stop-symbolic.svg",

		"IconNameNavigateBack":  "symbolic/actions/go-previous-symbolic.svg",
		"IconNameMoveDown":      "symbolic/actions/go-down-symbolic.svg",
		"IconNameNavigateNext":  "symbolic/actions/go-next-symbolic.svg",
		"IconNameMoveUp":        "symbolic/actions/go-up-symbolic.svg",
		"IconNameArrowDropDown": "symbolic/actions/go-down-symbolic.svg",
		"IconNameArrowDropUp":   "symbolic/actions/go-up-symbolic.svg",

		"IconNameFile":            "scalable/mimetypes/application-x-generic.svg",
		"IconNameFileApplication": "scalable/mimetypes/application-x-executable.svg",
		"IconNameFileAudio":       "scalable/mimetypes/audio-x-generic.svg",
		"IconNameFileImage":       "scalable/mimetypes/image-x-generic.svg",
		"IconNameFileText":        "scalable/mimetypes/text-x-generic.svg",
		"IconNameFileVideo":       "scalable/mimetypes/video-x-generic.svg",
		"IconNameFolder":          "scalable/places/folder.svg",
		"IconNameFolderNew":       "symbolic/actions/folder-new-symbolic.svg",
		"IconNameFolderOpen":      "symbolic/status/folder-open-symbolic.svg",
		"IconNameHelp":            "symbolic/actions/help-about-symbolic.svg",
		"IconNameHistory":         "",
		"IconNameHome":            "symbolic/places/user-home-symbolic.svg",
		"IconNameSettings":        "symbolic/categories/applications-system-symbolic.svg",

		"IconNameViewFullScreen": "symbolic/actions/view-fullscreen-symbolic.svg",
		"IconNameViewRefresh":    "symbolic/actions/view-refresh-symbolic.svg",
		"IconNameViewRestore":    "symbolic/actions/view-restore-symbolic.svg",
		"IconNameViewZoomFit":    "symbolic/actions/zoom-fit-best-symbolic.svg",
		"IconNameViewZoomIn":     "symbolic/actions/zoom-in-symbolic.svg",
		"IconNameViewZoomOut":    "symbolic/actions/zoom-out-symbolic.svg",

		"IconNameVisibility":    "symbolic/actions/view-reveal-symbolic.svg",
		"IconNameVisibilityOff": "symbolic/actions/view-conceal-symbolic.svg",

		"IconNameVolumeDown": "symbolic/status/audio-volume-low-symbolic.svg",
		"IconNameVolumeMute": "symbolic/status/audio-volume-muted-symbolic.svg",
		"IconNameVolumeUp":   "symbolic/status/audio-volume-high-symbolic.svg",

		"IconNameDownload": "symbolic/places/folder-download-symbolic.svg",
		"IconNameComputer": "symbolic/devices/computer-symbolic.svg",
		"IconNameStorage":  "symbolic/devices/drive-harddisk-symbolic.svg",
		"IconNameUpload":   "symbolic/actions/send-to-symbolic.svg",

		"IconNameAccount": "symbolic/status/avatar-default-symbolic.svg",
		"IconNameLogin":   "",
		"IconNameLogout":  "symbolic/actions/system-log-out-symbolic.svg",

		"IconNameList": "symbolic/actions/view-list-symbolic.svg",
		"IconNameGrid": "symbolic/actions/view-grid-symbolic.svg",
	}
	inkscape, _ = exec.LookPath("inkscape")
	forcePNG    = map[string]bool{
		"IconNameFileAudio":       true,
		"IconNameFileApplication": true,
	}
)

type colorInfo struct {
	Col     color.Color // go formated color (color.RGBA{0x00, 0x00, 0x00, 0x00})
	AdwName string      // Adwaita color name from the documentation without the "@"
}
type iconInfo struct {
	StaticName string // the theme name of the icon for Fyne
	Content    string // the content of the icon, SVG content
}

func main() {

	if err := generateColorScheme(); err != nil {
		log.Fatal(err)
	}

	if err := generateIcons(); err != nil {
		log.Fatal(err)
	}

}

// generateColorScheme generates the color scheme file from the Adwaita documentation. It downloads the documentation
// page and parse it to get the color scheme. Following the documentation and recommendations, it generates the light and dark
// color schemes.
func generateColorScheme() error {

	rows := [][]string{}
	darkScheme := map[string]colorInfo{}
	lightScheme := map[string]colorInfo{}

	reps, err := http.Get(adwaitaColorPage)
	if err != nil {
		return fmt.Errorf("failed to get page: %w", err)
	}
	defer reps.Body.Close()
	htpage, err := io.ReadAll(reps.Body)
	if err != nil {
		return fmt.Errorf("failed to read body: %w", err)
	}
	// find all the rows in the tables
	rows = tableRowMatcher.FindAllStringSubmatch(string(htpage), -1)

	// inline function, to get the color for a specific name and variant
	getWidgetColorFor := func(name, variant string) (col color.RGBA, err error) {
		for _, row := range rows {
			// check if the row is for "@success_color" (@ is html encoded)
			if strings.Contains(row[0], "&#64;"+name) || strings.Contains(row[0], "@"+name) {
				// the color is in the second column
				c := tableColorCellMatcher.FindAllStringSubmatch(row[0], -1)
				switch variant {
				case "light":
					col, err = stringToColor(c[0][1])
				case "dark":
					col, err = stringToColor(c[1][1])
				}
				return
			}
		}
		return
	}

	getStandardColorFor := func(name string) (col color.RGBA, err error) {
		for _, row := range rows {
			// check if the row is for "@success_color" (@ is html encoded)
			if strings.Contains(row[0], "&#64;"+name) || strings.Contains(row[0], "@"+name) {
				// the color is in the second column
				c := tableColorCellMatcher.FindAllStringSubmatch(row[0], -1)
				col, err = stringToColor(c[0][1])
				return
			}
		}
		return
	}

	for colname, color := range colorToGet {
		lcol, err := getWidgetColorFor(color, "light")
		if err != nil {
			return fmt.Errorf("failed to get light color for %s: %w", color, err)
		}
		dcol, err := getWidgetColorFor(color, "dark")
		if err != nil {
			return fmt.Errorf("failed to get dark color for %s: %w", color, err)
		}
		lightScheme[colname] = colorInfo{
			Col:     lcol,
			AdwName: color,
		}
		darkScheme[colname] = colorInfo{
			Col:     dcol,
			AdwName: color,
		}
	}

	for colname, color := range standardColorToGet {
		lightColorName := color
		darkColorName := color
		colors := strings.Split(color, ",")
		if len(colors) == 2 {
			lightColorName = colors[0]
			darkColorName = colors[1]
		}

		lcol, err := getStandardColorFor(lightColorName)
		if err != nil {
			return fmt.Errorf("failed to get light color for %s: %w", lightColorName, err)
		}
		dcol, err := getStandardColorFor(darkColorName)
		if err != nil {
			return fmt.Errorf("failed to get dark color for %s: %w", darkColorName, err)
		}

		// special case, alpha channel is 0x5b for scrollbar
		if colname == "theme.ColorNameScrollBar" {
			lcol.A = 0x5b
			dcol.A = 0x5b
		}

		lightScheme[colname] = colorInfo{
			Col:     lcol,
			AdwName: lightColorName,
		}
		darkScheme[colname] = colorInfo{
			Col:     dcol,
			AdwName: darkColorName,
		}
	}

	tpl := template.New("AdwaitaColorScheme")
	tpl, err = tpl.Parse(colorSourceTpl)
	if err != nil {
		return fmt.Errorf("failed to parse template: %w", err)
	}
	// generate the source
	buffer := bytes.NewBuffer(nil)
	err = tpl.Execute(buffer, struct {
		LightScheme map[string]colorInfo
		DarkScheme  map[string]colorInfo
	}{
		LightScheme: lightScheme,
		DarkScheme:  darkScheme,
	})
	if err != nil {
		return fmt.Errorf("failed to execute template: %w", err)
	}

	// format the file
	if formatted, err := format.Source(buffer.Bytes()); err != nil {
		return fmt.Errorf("failed to format source: %w", err)
	} else {
		return os.WriteFile(colorSchemeOutput, formatted, 0644)
	}
}

// generateIcons generates the icons file from the Adwaita icon theme. It downloads the theme from the gitlab repository
// as a tar file and extracts it in a temporary directory.
// Then, using the icons map, it get the corresponding icon and generate the go file.
func generateIcons() error {

	// get and untar the Adwaita icons from gitlab repository
	resp, err := http.Get(adwaitaIconsPage)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	tarReader := tar.NewReader(resp.Body)

	// extract in a temporary directory
	tmpDir, err := os.MkdirTemp("", "adwaita")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmpDir)

	// extract the tar file
	for {
		header, err := tarReader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err

		}
		if header.Typeflag == tar.TypeReg {
			target := filepath.Join(tmpDir, header.Name)
			if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
				return err
			}
			file, err := os.Create(target)
			if err != nil {
				return err
			}
			defer file.Close()
			if _, err := io.Copy(file, tarReader); err != nil {
				return err
			}
		}
	}

	// get the wanted icons
	icons := map[string]iconInfo{}
	for name, iconfile := range iconsToGet {
		if iconfile == "" {
			continue
		}

		iconPath := filepath.Join(tmpDir, "adwaita-icon-theme-master-Adwaita", "Adwaita", iconfile)
		if _, ok := forcePNG[name]; ok {
			svgToPng(iconPath)
			iconPath = strings.TrimSuffix(iconPath, ".svg") + ".png"
		}

		res, err := fyne.LoadResourceFromPath(iconPath)
		if err != nil {
			log.Println("Error bundeling", name, "from", iconPath, ":", err)
			continue
		}

		filecontent := res.Content()

		icons[name] = iconInfo{
			StaticName: filepath.Base(iconPath),
			Content:    string(filecontent),
		}
	}

	tpl := template.New("AdwaitaIcons")
	tpl = tpl.Funcs(template.FuncMap{
		"contains": strings.Contains,
	})
	tpl, err = tpl.Parse(iconSourceTpl)
	if err != nil {
		return fmt.Errorf("Error parsing template: %w", err)
	}
	// generate the source
	buffer := bytes.NewBuffer(nil)
	err = tpl.Execute(buffer, struct {
		Icons map[string]iconInfo
	}{
		Icons: icons,
	})
	if err != nil {
		return fmt.Errorf("Error executing template: %w", err)
	}

	if formatted, err := format.Source(buffer.Bytes()); err != nil {
		return fmt.Errorf("error formatting source: %w", err)
	} else {
		return os.WriteFile(iconsOutput, formatted, 0644)
	}
}

// svgToPng converts an SVG file to a PNG file using inkscape.
//
// TODO: fix oksvg to support all SVG files.
func svgToPng(filename string) error {
	// use inkscape to fix SVG files
	if inkscape == "" {
		return fmt.Errorf("inkscape not found")
	}
	log.Println("Converting", filename, "to PNG")
	cmd := exec.Command(
		inkscape,
		"--export-type=png",
		"--export-area-drawing",
		"--vacuum-defs",
		filename)
	return cmd.Run()
}

// stringToColor converts a string to a color.RGBA
func stringToColor(s string) (c color.RGBA, err error) {
	c.A = 0xff
	switch len(s) {
	case 7:
		_, err = fmt.Sscanf(s, "#%02x%02x%02x", &c.R, &c.G, &c.B)
	case 9:
		_, err = fmt.Sscanf(s, "#%02x%02x%02x%02x", &c.R, &c.G, &c.B, &c.A)
	default:
		// rgba(...) format
		var a float32
		_, err = fmt.Sscanf(s, "rgba(%d, %d, %d, %f)", &c.R, &c.G, &c.B, &a)
		c.A = uint8(a * 255)
	}
	return
}
