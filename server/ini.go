package server

import (
	"os"
	"strings"
)

// MergeINIFile returns the lines of a php.ini file with ini after them for
// [PHPConfig.INI], so that ini wins.
//
// A [PATH=...] or [HOST=...] section holds every line after it: PHP has no
// way back to the main settings, and a [PHP] line does not end it. Such
// sections, from the first to the end of the file, go after ini instead.
func MergeINIFile(path string, ini []string) ([]string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	lines := strings.Split(string(b), "\n")
	main := len(lines)
	for i, line := range lines {
		if iniSpecialSection(line) {
			main = i
			break
		}
	}
	out := append([]string{}, lines[:main]...)
	out = append(out, ini...)
	return append(out, lines[main:]...), nil
}

// iniSpecialSection reports whether line starts a section that PHP applies
// to some paths or hosts only. PHP matches the prefix case-insensitively,
// after it removes the quotes of ["PATH=/x"] or ['PATH=/x']. Spaces count
// only before a quote: [ PATH=/x] is an ordinary section.
//
//declscope:shared // pool.go puts its enforced entries before the sections
func iniSpecialSection(line string) bool {
	name, ok := strings.CutPrefix(strings.TrimSpace(line), "[")
	if !ok {
		return false
	}
	if quoted := strings.TrimLeft(name, " \t"); strings.HasPrefix(quoted, `"`) || strings.HasPrefix(name, "'") {
		name = quoted[1:]
	}
	if len(name) < 4 {
		return false
	}
	prefix := strings.ToUpper(name[:4])
	return prefix == "PATH" || prefix == "HOST"
}
