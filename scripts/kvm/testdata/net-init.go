// Static PID 1 of a disposable L2 network test guest.
package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

func run() error {
	for _, fs := range []struct{ kind, dir string }{{"proc", "/proc"}, {"sysfs", "/sys"}, {"devtmpfs", "/dev"}} {
		if err := syscall.Mount(fs.kind, fs.dir, fs.kind, 0, ""); err != nil {
			return err
		}
	}
	console, err := os.OpenFile("/dev/console", os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	os.Stdout, os.Stderr = console, console
	for _, driver := range []string{"virtio_pci", "virtio_net"} {
		// A built-in driver has no module. Check the actual interface below.
		_ = exec.Command("modprobe", driver).Run()
	}
	data, err := os.ReadFile("/proc/cmdline")
	if err != nil {
		return err
	}
	cfg := map[string]string{}
	for _, arg := range strings.Fields(string(data)) {
		if key, val, ok := strings.Cut(arg, "="); ok {
			cfg[key] = val
		}
	}
	for i := 0; i < 100; i++ {
		if _, err := os.Stat("/sys/class/net/eth0"); err == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	for _, args := range [][]string{{"link", "set", "eth0", "up"}, {"addr", "add", cfg["addr"], "dev", "eth0"}, {"route", "add", "default", "via", cfg["gw"]}} {
		if out, err := exec.Command("ip", args...).CombinedOutput(); err != nil {
			return fmt.Errorf("ip %v: %w %s", args, err, out)
		}
	}
	token := cfg["token"]
	if cfg["role"] == "peer" {
		ln, err := net.Listen("tcp4", ":8081")
		if err != nil {
			return err
		}
		fmt.Println("PULSE_CH_PEER_READY")
		return http.Serve(ln, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, token) }))
	}
	dial := net.Dialer{Timeout: 5 * time.Second}
	dial.Resolver = &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, net.JoinHostPort(cfg["dns"], "53"))
	}}
	client := http.Client{Timeout: 15 * time.Second, Transport: &http.Transport{DialContext: dial.DialContext}}
	for _, key := range []string{"peer", "outer", "egress"} {
		resp, err := client.Get(cfg[key])
		if err != nil {
			return fmt.Errorf("%s: %w", key, err)
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		if err != nil || resp.StatusCode != 200 || (key != "egress" && string(body) != token) {
			return fmt.Errorf("%s failed, status %d", key, resp.StatusCode)
		}
	}
	fmt.Println("PULSE_CH_NESTED_NETWORK_PASS")
	return nil
}

func main() {
	if os.Getpid() != 1 {
		fmt.Fprintln(os.Stderr, "expected PID 1 inside a scratch guest")
		os.Exit(1)
	}
	os.Setenv("PATH", "/usr/sbin:/sbin:/usr/bin:/bin")
	if err := run(); err != nil {
		fmt.Println("network test failed:", err)
	}
	syscall.Reboot(syscall.LINUX_REBOOT_CMD_POWER_OFF)
	select {}
}
