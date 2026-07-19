// netinfo.go — discovering the gateway's public-facing IP addresses.
// A cloud instance often has BOTH an IPv4 and an IPv6, and outbound
// mail may leave over either — so SPF has to cover every one of them,
// and each needs working reverse DNS. The gateway reports the full set
// to PCP (status response), which builds the SPF record and per-IP PTR
// checks from it.
package postoffice

import (
	"net"
	"sort"
)

// publicIPs returns the gateway's global-unicast (public) IP addresses
// across all interfaces — both IPv4 and IPv6, deduplicated and sorted.
// Loopback, link-local, and private/ULA addresses are excluded. On a
// cloud VM with directly-attached public IPs (the common case) this is
// exactly the set mail can leave from.
func publicIPs() []string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			var ip net.IP
			switch v := a.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			}
			if ip == nil || !ip.IsGlobalUnicast() || ip.IsPrivate() {
				continue
			}
			s := ip.String()
			if !seen[s] {
				seen[s] = true
				out = append(out, s)
			}
		}
	}
	sort.Strings(out)
	return out
}
