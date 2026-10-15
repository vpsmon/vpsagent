package service

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestReadyNotification(t *testing.T) {
	t.Setenv("NOTIFY_SOCKET", "")
	if err := NotifyReady(); err != nil {
		t.Fatal(err)
	}
	dir, err := os.MkdirTemp("/tmp", "vpsagent-notify-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "notify.sock")
	listener, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: path, Net: "unixgram"})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	t.Setenv("NOTIFY_SOCKET", path)
	if err := NotifyReady(); err != nil {
		t.Fatal(err)
	}
	if err := listener.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	data := make([]byte, 1024)
	n, _, err := listener.ReadFromUnix(data)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(data[:n]), "READY=1\nSTATUS=") {
		t.Fatalf("notification=%q", data[:n])
	}
	t.Setenv("NOTIFY_SOCKET", filepath.Join(dir, "missing.sock"))
	if err := NotifyReady(); err == nil {
		t.Fatal("missing notification socket accepted")
	}
}
