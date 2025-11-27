package main

import (
	"fmt"
	"net"
)

func detectOutboundIP() (string, error) {
	conn, err := net.Dial("udp", "8.8.8.8:80")
	if err != nil {
		return "", fmt.Errorf("dial udp: %w", err)
	}
	defer conn.Close()

	localAddr, ok := conn.LocalAddr().(*net.UDPAddr)
	if !ok || localAddr.IP == nil {
		return "", fmt.Errorf("unexpected local address type %T", conn.LocalAddr())
	}
	return localAddr.IP.String(), nil
}
