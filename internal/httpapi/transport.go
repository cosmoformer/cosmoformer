package httpapi

import (
	"context"
	"net"
	"net/http"
	"net/netip"
)

const clientIPHeader = "X-Cosmoformer-Client-IP"

type unixConnectionKey struct{}

// ConnectionContext records the actual transport, independently of HTTP fields.
// Only the owner-protected Unix listener may carry proxy client metadata.
func ConnectionContext(ctx context.Context, conn net.Conn) context.Context {
	_, unix := conn.RemoteAddr().(*net.UnixAddr)
	return context.WithValue(ctx, unixConnectionKey{}, unix)
}

func authenticationClient(r *http.Request) (string, bool) {
	if unix, _ := r.Context().Value(unixConnectionKey{}).(bool); unix {
		values := r.Header.Values(clientIPHeader)
		if len(values) == 0 {
			// Direct socket callers already hold the host owner's authority. Keep
			// their admission separate from browser clients forwarded by the proxy.
			return "unix:operator", true
		}
		if len(values) != 1 {
			return "", false
		}
		ip, err := netip.ParseAddr(values[0])
		if err != nil || ip.Zone() != "" || ip.IsUnspecified() {
			return "", false
		}
		return "proxy:" + ip.Unmap().String(), true
	}
	// TCP clients cannot select their identity through any forwarding header.
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return "", false
	}
	ip, err := netip.ParseAddr(host)
	if err != nil || ip.Zone() != "" || ip.IsUnspecified() {
		return "", false
	}
	return "peer:" + ip.Unmap().String(), true
}
