// Package units formats sizes the way all of rpak's output shows them.
package units

import "fmt"

// Size formats a size as flatpak does (g_format_size): decimal units,
// "412.3 MB".
func Size(n int64) string {
	if n < 1000 {
		if n == 1 {
			return "1 byte"
		}
		return fmt.Sprintf("%d bytes", n)
	}
	v := float64(n)
	for _, unit := range []string{"kB", "MB", "GB", "TB", "PB"} {
		v /= 1000
		if v < 999.95 {
			return fmt.Sprintf("%.1f %s", v, unit)
		}
	}
	return fmt.Sprintf("%.1f EB", v/1000)
}
