package main

import (
	"net"
	"testing"
)

func TestRestrictedSSHRecoveryInterfacesFindLoopback(t *testing.T) {
	interfaces, err := restrictedSSHRecoveryInterfaces()
	if err != nil {
		t.Fatal(err)
	}
	for _, iface := range interfaces {
		if !iface.IsLoopback() {
			continue
		}
		for _, addr := range iface.AltAddrs {
			if ip, ok := addr.(*net.IPNet); ok && ip.IP.To4() != nil && ip.IP.IsLoopback() {
				return
			}
		}
	}
	t.Fatal("IPv4 loopback missing from restricted enumeration")
}
