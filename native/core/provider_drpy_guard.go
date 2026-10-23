package core

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
)

var errDrpyPrivateTarget = errors.New("脚本请求被拒绝：不允许访问本机或局域网地址")

var drpyAllowPrivateTargets = false

var drpyBlockedNetworks = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("fc00::/7"),
}

func drpyPublicAddress(address netip.Addr) bool {
	address = address.Unmap()
	if !address.IsValid() ||
		address.IsUnspecified() ||
		address.IsLoopback() ||
		address.IsPrivate() ||
		address.IsLinkLocalUnicast() ||
		address.IsLinkLocalMulticast() ||
		address.IsInterfaceLocalMulticast() ||
		address.IsMulticast() {
		return false
	}
	for _, network := range drpyBlockedNetworks {
		if network.Contains(address) {
			return false
		}
	}
	return true
}

func drpyCheckTarget(ctx context.Context, target *url.URL) error {
	if target == nil {
		return errDrpyPrivateTarget
	}
	if drpyAllowPrivateTargets {
		return nil
	}
	if scheme := strings.ToLower(target.Scheme); scheme != "http" && scheme != "https" {
		return errors.New("脚本请求被拒绝：只允许 http / https 地址")
	}
	host := strings.ToLower(strings.TrimSuffix(target.Hostname(), "."))
	if host == "" || host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return errDrpyPrivateTarget
	}
	if address, err := netip.ParseAddr(host); err == nil {
		if !drpyPublicAddress(address) {
			return errDrpyPrivateTarget
		}
		return nil
	}
	addresses, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return nil
	}
	for _, address := range addresses {
		if !drpyPublicAddress(address) {
			return errDrpyPrivateTarget
		}
	}
	return nil
}

type untrustedTargetKey struct{}

func withUntrustedTargets(ctx context.Context) context.Context {
	return context.WithValue(ctx, untrustedTargetKey{}, true)
}

func checkUntrustedTarget(request *http.Request) error {
	if untrusted, _ := request.Context().Value(untrustedTargetKey{}).(bool); !untrusted {
		return nil
	}
	return drpyCheckTarget(request.Context(), request.URL)
}

func drpyGuardedClient(ctx context.Context, client *http.Client) *http.Client {
	scoped := *client
	previous := client.CheckRedirect
	scoped.CheckRedirect = func(request *http.Request, via []*http.Request) error {
		if err := drpyCheckTarget(ctx, request.URL); err != nil {
			return err
		}
		if previous != nil {
			return previous(request, via)
		}
		if len(via) >= 10 {
			return errors.New("脚本请求重定向过多")
		}
		return nil
	}
	return &scoped
}
