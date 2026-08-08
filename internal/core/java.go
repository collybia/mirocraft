package core

import (
	"regexp"
	"strconv"
	"strings"
)

// Java majors the panel installs. Four cover every Minecraft version in use,
// so the runtime manager has four downloads to care about rather than one per
// Minecraft release.
const (
	Java8  = 8
	Java17 = 17
	Java21 = 21
	Java25 = 25
)

// DefaultJavaMajor is used when a version cannot be placed at all. It is the
// newest, because an unrecognisable id is far more likely to be a version
// released after this table was written than something from 2013.
const DefaultJavaMajor = Java25

// calendarEra is the first major number of the calendar-versioned era.
// Minecraft numbered releases 1.x until 1.21.11 (December 2025) and switched
// to YY.N with 26.1 (March 2026), so a major of 1 means the old scheme and
// anything larger means the new one.
const calendarEra = 26

// numericPrefix matches the leading numeric part of a version, ignoring any
// suffix: 1.21.4, 26.2, and the leading 26.3 of 26.3-snapshot-7.
var numericPrefix = regexp.MustCompile(`^(\d+)\.(\d+)(?:\.(\d+))?`)

// weeklySnapshot matches the retired weekly snapshot form, for example 24w45a.
// Snapshots have used the YY.N-snapshot-N form since the calendar switch, but
// the old ids remain in Mojang's manifest forever.
var weeklySnapshot = regexp.MustCompile(`^(\d{2})w(\d{2})[a-z]$`)

// JavaMajorFor returns the Java version a Minecraft version needs.
//
// The boundaries below were read off Mojang's own per-version manifests
// rather than guessed:
//
//	1.16.5  -> 8
//	1.17.1  -> 16   (the panel installs 17; see below)
//	1.20.4  -> 17
//	1.20.6  -> 21
//	1.21.11 -> 21
//	26.1.2  -> 25
//	26.2    -> 25
//
// 1.17 is the one place this deliberately disagrees with upstream: Mojang
// asks for Java 16, and the panel installs 17, which runs 1.17 correctly.
// Carrying a whole Java 16 runtime that nothing else needs is not worth it.
//
// This is a fallback. Where upstream publishes the requirement — the Mojang
// manifest does, per version — that value wins, because it cannot go stale
// the way a table in this repository can. The 26.x line only exists here
// because Paper's API does not state a Java requirement at all.
func JavaMajorFor(version string) int {
	version = strings.TrimSpace(version)

	if m := numericPrefix.FindStringSubmatch(version); m != nil {
		major, err := strconv.Atoi(m[1])
		if err != nil {
			return DefaultJavaMajor
		}

		// Calendar-versioned releases: 26.1 onwards.
		if major >= calendarEra {
			return Java25
		}
		// A major between 2 and 25 was never released. Treating it as new
		// rather than ancient is the safer guess if one ever appears.
		if major != 1 {
			return DefaultJavaMajor
		}

		minor, err := strconv.Atoi(m[2])
		if err != nil {
			return DefaultJavaMajor
		}
		patch := 0
		if m[3] != "" {
			if p, err := strconv.Atoi(m[3]); err == nil {
				patch = p
			}
		}

		switch {
		case minor < 17:
			return Java8
		case minor < 20:
			return Java17
		case minor == 20 && patch < 5:
			return Java17
		default:
			return Java21
		}
	}

	if m := weeklySnapshot.FindStringSubmatch(version); m != nil {
		year, err := strconv.Atoi(m[1])
		if err != nil {
			return DefaultJavaMajor
		}
		// Weekly snapshots are dated, so the year places them: 24w and 25w
		// fall in the Java 21 era, 21w–23w in the Java 17 era, and anything
		// older predates both.
		switch {
		case year >= 24:
			return Java21
		case year >= 21:
			return Java17
		default:
			return Java8
		}
	}

	return DefaultJavaMajor
}

// IsRelease reports whether an id looks like a finished release rather than a
// snapshot, release candidate or pre-release.
//
// Upstream marks the channel where it can, but Paper's version list does not,
// and putting somebody on 26.2-rc-2 because it sorts highest would be a poor
// default.
func IsRelease(version string) bool {
	version = strings.TrimSpace(version)
	if version == "" {
		return false
	}
	if weeklySnapshot.MatchString(version) {
		return false
	}
	if numericPrefix.FindString(version) == "" {
		return false
	}
	// Everything after the numeric prefix marks a pre-release of some kind:
	// -rc-2, -rc3, -pre5, -snapshot-7.
	return numericPrefix.FindString(version) == version
}

// CompareVersions orders two Minecraft version ids. It returns a negative
// number when a precedes b, zero when they are equal and a positive number
// otherwise.
//
// The comparison is numeric per component, so 1.10 correctly sorts above 1.9,
// and the calendar era sorts above the 1.x era because 26 > 1. A version with
// a suffix sorts just below the same version without one, which puts 26.2
// above 26.2-rc-2.
func CompareVersions(a, b string) int {
	pa, sa := parseVersion(a)
	pb, sb := parseVersion(b)

	for i := range pa {
		if pa[i] != pb[i] {
			return pa[i] - pb[i]
		}
	}

	switch {
	case sa == "" && sb == "":
		return 0
	case sa == "":
		return 1 // a release outranks its own pre-releases
	case sb == "":
		return -1
	default:
		return strings.Compare(sa, sb)
	}
}

// parseVersion splits an id into its numeric components and whatever suffix
// follows them. An id with no numeric prefix at all sorts below everything.
func parseVersion(version string) ([3]int, string) {
	version = strings.TrimSpace(version)

	m := numericPrefix.FindStringSubmatch(version)
	if m == nil {
		return [3]int{-1, -1, -1}, version
	}

	var out [3]int
	for i := 1; i <= 3; i++ {
		if m[i] == "" {
			continue
		}
		if n, err := strconv.Atoi(m[i]); err == nil {
			out[i-1] = n
		}
	}
	return out, strings.TrimPrefix(version, m[0])
}
