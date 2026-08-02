package central

import (
	"bufio"
	"net"
	"os"
	"runtime"
	"strconv"
	"strings"
)

// Field limits enforced by webui-fastify/src/validation/agent.ts. It answers
// 400 rather than truncating, so a long hostname would fail registration
// outright — truncate here instead.
const (
	maxHostname = 255
	maxShort    = 64
	maxIPAddrs  = 64
	maxCPUs     = 4096
)

// Collect describes this host for the registration record. Everything here is
// display-only: the console never gates a decision on it, so a field it cannot
// determine is simply omitted rather than guessed.
func Collect(version string) SystemInfo {
	info := SystemInfo{
		OS:      truncate(runtime.GOOS, maxShort),
		Arch:    truncate(runtime.GOARCH, maxShort),
		CPUs:    min(runtime.NumCPU(), maxCPUs),
		Version: truncate(version, maxShort),
		IPAddrs: localAddrs(),
	}
	if h, err := os.Hostname(); err == nil {
		info.Hostname = truncate(h, maxHostname)
	}
	info.MemBytes = totalMemory()
	return info
}

// localAddrs lists this host's own addresses, skipping loopback and
// link-local — an operator matching a pending gateway to a machine needs the
// addresses that identify it, not 127.0.0.1 on every box.
func localAddrs() []string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil
	}
	out := make([]string, 0, len(addrs))
	for _, a := range addrs {
		ipnet, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		ip := ipnet.IP
		if ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
			continue
		}
		out = append(out, truncate(ip.String(), maxShort))
		if len(out) == maxIPAddrs {
			break
		}
	}
	return out
}

// totalMemory returns the machine's RAM in bytes, or 0 to omit the field.
//
// Go has no portable total-memory call. Reading /proc/meminfo covers Linux,
// which is the only platform this ships on; anywhere else the field is left
// out rather than invented, because a wrong number on an inventory screen is
// worse than a blank one.
func totalMemory() int64 {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "MemTotal:") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			return 0
		}
		kb, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil {
			return 0
		}
		return kb * 1024
	}
	return 0
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
