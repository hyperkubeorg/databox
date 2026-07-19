// httprange.go — the single-range Range-header parser every blob-serving
// surface shares (Drive downloads, the public share raw endpoint, the
// /api/v1 download; media streaming joins in phase 6).
package kernel

import (
	"strconv"
	"strings"
)

// ParseRange handles the single-range form "bytes=a-b" (plus the open
// "a-" and suffix "-n" variants) — all a media element or a sane API
// client ever sends. Multi-range requests report !ok and the caller
// answers 416.
func ParseRange(header string, size int64) (start, end int64, ok bool) {
	spec, found := strings.CutPrefix(header, "bytes=")
	if !found || strings.Contains(spec, ",") || size <= 0 {
		return 0, 0, false
	}
	lo, hi, found := strings.Cut(strings.TrimSpace(spec), "-")
	if !found {
		return 0, 0, false
	}
	switch {
	case lo == "" && hi != "": // suffix: last n bytes
		n, err := strconv.ParseInt(hi, 10, 64)
		if err != nil || n <= 0 {
			return 0, 0, false
		}
		if n > size {
			n = size
		}
		return size - n, size - 1, true
	case lo != "":
		s, err := strconv.ParseInt(lo, 10, 64)
		if err != nil || s < 0 || s >= size {
			return 0, 0, false
		}
		e := size - 1
		if hi != "" {
			e, err = strconv.ParseInt(hi, 10, 64)
			if err != nil || e < s {
				return 0, 0, false
			}
			if e >= size {
				e = size - 1
			}
		}
		return s, e, true
	}
	return 0, 0, false
}
