// Package service implements the small systemd readiness protocol without an
// external executable. Standalone runs have no notification socket.
package service

import (
	"net"
	"os"
	"time"
)

func NotifyReady() error {
	address := os.Getenv("NOTIFY_SOCKET")
	if address == "" {
		return nil
	}
	conn, err := net.DialUnix("unixgram", nil, &net.UnixAddr{Name: address, Net: "unixgram"})
	if err != nil {
		return err
	}
	defer conn.Close()
	if err := conn.SetWriteDeadline(time.Now().Add(time.Second)); err != nil {
		return err
	}
	_, err = conn.Write([]byte("READY=1\nSTATUS=Fresh host metrics collected; telemetry transport started"))
	return err
}
