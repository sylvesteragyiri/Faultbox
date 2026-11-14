package redismon

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"testing"

	"github.com/sylvesteragyiri/faultbox/internal/auth"
	"github.com/sylvesteragyiri/faultbox/internal/store"
)

func TestParseInfo(t *testing.T) {
	got, err := parseInfo("# Memory\nused_memory:8388608\nmaxmemory:10485760\n# Clients\nconnected_clients:12\nuptime_in_seconds:900\n# Keyspace\ndb0:keys=8,expires=2,avg_ttl=10\ndb2:keys=3,expires=1,avg_ttl=20\n")
	if err != nil {
		t.Fatal(err)
	}
	if got.UsedMemory != 8<<20 || got.MaxMemory != 10<<20 || got.ConnectedClients != 12 || got.KeyCount != 11 || got.UptimeSeconds != 900 {
		t.Fatalf("parsed INFO = %+v", got)
	}
}

func TestParseInfoRejectsMissingAndInvalidMetrics(t *testing.T) {
	for name, raw := range map[string]string{
		"missing": "maxmemory:0\nconnected_clients:0\nuptime_in_seconds:1\n",
		"invalid": "used_memory:-1\nmaxmemory:0\nconnected_clients:0\nuptime_in_seconds:1\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := parseInfo(raw); err == nil {
				t.Fatal("expected invalid INFO to fail")
			}
		})
	}
}

func TestReadInfoUsesRedisRESPAndParsesMetrics(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	done := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			done <- err
			return
		}
		defer func() { _ = conn.Close() }()
		reader := bufio.NewReader(conn)
		var args []string
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				done <- err
				return
			}
			if strings.HasPrefix(line, "*") {
				continue
			}
			if strings.HasPrefix(line, "$1") {
				continue
			}
			if strings.HasPrefix(line, "$4") {
				value := make([]byte, 6)
				if _, err := reader.Read(value); err != nil {
					done <- err
					return
				}
				args = append(args, strings.TrimSpace(string(value)))
				if len(args) == 1 {
					break
				}
			}
		}
		if args[0] != "INFO" {
			done <- fmt.Errorf("command = %q", args[0])
			return
		}
		info := "used_memory:100\nmaxmemory:200\nconnected_clients:3\nuptime_in_seconds:4\n"
		_, err = fmt.Fprintf(conn, "$%d\r\n%s\r\n", len(info), info)
		done <- err
	}()
	port := listener.Addr().(*net.TCPAddr).Port
	got, err := ReadInfo(context.Background(), "redis://127.0.0.1:"+strconv.Itoa(port)+"/0")
	if err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if got.UsedMemory != 100 || got.MaxMemory != 200 || got.ConnectedClients != 3 {
		t.Fatalf("sample = %+v", got)
	}
}

func TestReadInfoRejectsUnsupportedEndpoint(t *testing.T) {
	if _, err := ReadInfo(context.Background(), "http://localhost:6379"); err == nil {
		t.Fatal("expected unsupported scheme to fail")
	}
}

type alertCapture struct{ thresholds []int64 }

func (a *alertCapture) RedisMemoryAlert(_ context.Context, _ string, _, _, _, threshold int64) error {
	a.thresholds = append(a.thresholds, threshold)
	return nil
}

func TestCollectorAlertsOnCrossingsOnceAndRearmsAfterDrop(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, t.TempDir()+"/redis.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	key := make([]byte, 32)
	values := make(chan int64, 8)
	listener := startInfoServer(t, values)
	defer func() { _ = listener.Close() }()
	endpoint, err := auth.Seal(key, []byte("redis://"+listener.Addr().String()+"/0"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.SaveRedisTarget(ctx, store.RedisTarget{Name: "Main", Endpoint: endpoint, Enabled: true, AlertEnabled: true}); err != nil {
		t.Fatal(err)
	}

	alerts := &alertCapture{}
	c := New(st, key, 30, nil)
	c.SetAlertSender(alerts)
	for _, used := range []int64{790, 800, 850, 900, 1000, 950, 700, 800} {
		values <- used
		if err := c.Poll(ctx); err != nil {
			t.Fatal(err)
		}
	}
	want := []int64{80, 90, 100, 80}
	if fmt.Sprint(alerts.thresholds) != fmt.Sprint(want) {
		t.Fatalf("threshold alerts = %v, want %v", alerts.thresholds, want)
	}
}

func startInfoServer(t *testing.T, values <-chan int64) net.Listener {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = conn.Close() }()
				reader := bufio.NewReader(conn)
				var command string
				for {
					line, err := reader.ReadString('\n')
					if err != nil {
						return
					}
					if strings.HasPrefix(line, "$4") {
						b := make([]byte, 6)
						if _, err := io.ReadFull(reader, b); err != nil {
							return
						}
						command = strings.TrimSpace(string(b))
						break
					}
				}
				if command != "INFO" {
					return
				}
				used := <-values
				info := fmt.Sprintf("used_memory:%d\nmaxmemory:1000\nconnected_clients:1\nuptime_in_seconds:100\n", used)
				_, _ = fmt.Fprintf(conn, "$%d\r\n%s\r\n", len(info), info)
			}()
		}
	}()
	return listener
}
