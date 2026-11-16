package main

import (
	"encoding/hex"
	"errors"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"

	"golang.org/x/sys/unix"
	"tailscale.com/net/netmon"
)

var configureSSHRecoveryNetworkOnce sync.Once

func configureSSHRecoveryNetwork() {
	configureSSHRecoveryNetworkOnce.Do(func() {
		// The installed unit omits AF_NETLINK. Tailscale can poll for changes,
		// but its default interface enumeration also opens a Netlink socket.
		_, err := net.Interfaces()
		if errors.Is(err, unix.EAFNOSUPPORT) || errors.Is(err, unix.EPERM) || errors.Is(err, unix.EACCES) {
			netmon.RegisterInterfaceGetter(restrictedSSHRecoveryInterfaces)
		}
	})
}

func restrictedSSHRecoveryInterfaces() ([]netmon.Interface, error) {
	entries, err := os.ReadDir("/sys/class/net")
	if err != nil {
		return nil, err
	}
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	defer unix.Close(fd)

	v6, _ := os.ReadFile("/proc/net/if_inet6")
	ipv6 := sshRecoveryIPv6Addrs(string(v6))
	interfaces := make([]netmon.Interface, 0, len(entries))
	for _, entry := range entries {
		iface, err := readSSHRecoveryInterface(fd, entry.Name())
		if err != nil {
			// An interface may disappear between listing sysfs and querying it.
			if errors.Is(err, unix.ENODEV) {
				continue
			}
			return nil, err
		}
		iface.AltAddrs = append(iface.AltAddrs, ipv6[iface.Name]...)
		interfaces = append(interfaces, iface)
	}
	return interfaces, nil
}

func readSSHRecoveryInterface(fd int, name string) (netmon.Interface, error) {
	req, err := unix.NewIfreq(name)
	if err != nil {
		return netmon.Interface{}, err
	}
	iface := &net.Interface{Name: name}
	if err := unix.IoctlIfreq(fd, unix.SIOCGIFINDEX, req); err != nil {
		return netmon.Interface{}, err
	}
	iface.Index = int(req.Uint32())
	if err := unix.IoctlIfreq(fd, unix.SIOCGIFMTU, req); err != nil {
		return netmon.Interface{}, err
	}
	iface.MTU = int(req.Uint32())
	if err := unix.IoctlIfreq(fd, unix.SIOCGIFFLAGS, req); err != nil {
		return netmon.Interface{}, err
	}
	for _, flag := range []struct {
		kernel uint16
		goFlag net.Flags
	}{
		{unix.IFF_UP, net.FlagUp}, {unix.IFF_BROADCAST, net.FlagBroadcast},
		{unix.IFF_LOOPBACK, net.FlagLoopback}, {unix.IFF_POINTOPOINT, net.FlagPointToPoint},
		{unix.IFF_MULTICAST, net.FlagMulticast}, {unix.IFF_RUNNING, net.FlagRunning},
	} {
		if req.Uint16()&flag.kernel != 0 {
			iface.Flags |= flag.goFlag
		}
	}
	// A non-nil list keeps netmon from calling net.Interface.Addrs via Netlink.
	addrs := []net.Addr{}
	if err := unix.IoctlIfreq(fd, unix.SIOCGIFADDR, req); err == nil {
		ip, ipErr := req.Inet4Addr()
		if err := unix.IoctlIfreq(fd, unix.SIOCGIFNETMASK, req); err != nil {
			return netmon.Interface{}, err
		}
		mask, maskErr := req.Inet4Addr()
		if ipErr != nil || maskErr != nil {
			return netmon.Interface{}, errors.Join(ipErr, maskErr)
		}
		addrs = append(addrs, &net.IPNet{IP: net.IP(ip), Mask: net.IPMask(mask)})
	} else if !errors.Is(err, unix.EADDRNOTAVAIL) {
		return netmon.Interface{}, err
	}
	return netmon.Interface{Interface: iface, AltAddrs: addrs}, nil
}

func sshRecoveryIPv6Addrs(data string) map[string][]net.Addr {
	addrs := make(map[string][]net.Addr)
	for line := range strings.SplitSeq(data, "\n") {
		fields := strings.Fields(line)
		if len(fields) != 6 {
			continue
		}
		ip, ipErr := hex.DecodeString(fields[0])
		bits, bitsErr := strconv.ParseUint(fields[2], 16, 8)
		mask := net.CIDRMask(int(bits), 128)
		if ipErr != nil || bitsErr != nil || len(ip) != net.IPv6len || mask == nil {
			continue
		}
		name := fields[5]
		addrs[name] = append(addrs[name], &net.IPNet{IP: net.IP(ip), Mask: mask})
	}
	return addrs
}
