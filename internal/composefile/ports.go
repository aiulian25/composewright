package composefile

import (
	"strconv"
	"strings"
)

// portRangeSeparator parts the ends of a host port range such as 8000-8010.
const portRangeSeparator = "-"

// MaxPort is the highest TCP or UDP port.
const MaxPort = 65535

// HostPorts are the host ports a published port such as "8080" or a range
// such as "8000-8010" binds; none when Docker picks one, or when the text
// is no number, as an uninterpolated ${PORT}, or no ascending range of
// ports.
func HostPorts(published string) []int {
	start, end, isRange := strings.Cut(published, portRangeSeparator)
	if !isRange {
		end = start
	}
	first, startErr := strconv.Atoi(start)
	last, endErr := strconv.Atoi(end)
	valid := startErr == nil && endErr == nil && 1 <= first && first <= last && last <= MaxPort
	if !valid {
		return nil
	}
	var ports []int
	for port := first; port <= last; port++ {
		ports = append(ports, port)
	}
	return ports
}
