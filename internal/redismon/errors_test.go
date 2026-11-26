package redismon

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/sylvesteragyiri/faultbox/internal/auth"
	"github.com/sylvesteragyiri/faultbox/internal/store"
)

func TestCrossedThresholdReportsOnlyTheHighest(t *testing.T) {
	for _, tc := range []struct{ previous, current, want int64 }{
		{0, 79, 0},
		{79, 80, 80},
		{80, 89, 0},
		{70, 95, 90},
		{70, 100, 100},
		{100, 100, 0},
		{95, 85, 0},
	} {
		if got := crossedThreshold(tc.previous, tc.current); got != tc.want {
			t.Errorf("crossedThreshold(%d, %d) = %d, want %d", tc.previous, tc.current, got, tc.want)
		}
	}
}

func TestCollectorSendsOneAlertForAJumpPastSeveralThresholds(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, t.TempDir()+"/redis.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	key := make([]byte, 32)
	values := make(chan int64, 4)
	listener := startInfoServer(t, values)
	defer func() { _ = listener.Close() }()
	endpoint, err := auth.Seal(key, []byte("redis://"+listener.Addr().String()))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.SaveRedisTarget(ctx, store.RedisTarget{Name: "Main", Endpoint: endpoint, Enabled: true, AlertEnabled: true}); err != nil {
		t.Fatal(err)
	}
	alerts := &alertCapture{}
	c := New(st, key, nil)
	c.SetAlertSender(alerts)
	for _, used := range []int64{700, 1000} {
		values <- used
		if err := c.Poll(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if fmt.Sprint(alerts.thresholds) != "[100]" {
		t.Fatalf("threshold alerts = %v, want [100]", alerts.thresholds)
	}
}

func TestCollectorStoresAReadableFailureReason(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, t.TempDir()+"/redis.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	key := make([]byte, 32)
	listener := replyServer(t, "-NOAUTH Authentication required.\r\n")
	endpoint, err := auth.Seal(key, []byte("redis://"+listener.Addr().String()))
	if err != nil {
		t.Fatal(err)
	}
	id, err := st.SaveRedisTarget(ctx, store.RedisTarget{Name: "Main", Endpoint: endpoint, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := New(st, key, nil).Poll(ctx); err == nil {
		t.Fatal("expected the failed check to be reported")
	}
	target, err := st.RedisTarget(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if target.LastError != "Redis requires a password; add it to the URL." {
		t.Fatalf("stored reason = %q", target.LastError)
	}
}

func TestReadInfoExplainsRedisErrorReplies(t *testing.T) {
	for name, tc := range map[string]struct {
		endpoint, reply, want string
	}{
		"noauth":          {"redis://%s", "-NOAUTH Authentication required.\r\n", "redis requires a password; add it to the URL"},
		"noperm":          {"redis://%s", "-NOPERM this user has no permissions to run the 'info' command\r\n", "the Redis user is not allowed to run INFO"},
		"loading":         {"redis://%s", "-LOADING Redis is loading the dataset in memory\r\n", "redis is still loading its dataset"},
		"other":           {"redis://%s", "-ERR secret-server-text\r\n", "redis INFO failed"},
		"wrong password":  {"redis://:bad@%s", "-WRONGPASS invalid username-password pair\r\n", "redis rejected the username or password"},
		"no password set": {"redis://:pw@%s", "-ERR AUTH <password> called without any password configured for the default user.\r\n", "redis has no password set; remove the password from the URL"},
		"select range":    {"redis://%s/99", "-ERR DB index is out of range\r\n", "redis database number is out of range"},
		"plain to TLS":    {"redis://%s", "\x15\x03\x01\x00\x02\x02\x50\r\n", "unexpected Redis response; if the server requires TLS, use rediss://"},
		"closed early":    {"redis://%s", "", "redis closed the connection"},
		"oversized INFO":  {"redis://%s", "$99999999\r\n", "redis INFO response exceeds limit"},
		"malformed bulk":  {"redis://%s", "$2\r\nabXY", "invalid Redis bulk response"},
	} {
		t.Run(name, func(t *testing.T) {
			listener := replyServer(t, tc.reply)
			_, err := ReadInfo(context.Background(), fmt.Sprintf(tc.endpoint, listener.Addr().String()))
			if err == nil || err.Error() != tc.want {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
			if strings.Contains(err.Error(), "secret-server-text") || strings.Contains(err.Error(), "127.0.0.1") {
				t.Fatalf("error leaked server text or address: %v", err)
			}
		})
	}
}

func TestDialErrorClassifiesWithoutLeakingAddresses(t *testing.T) {
	opErr := func(err error) error {
		return &net.OpError{Op: "dial", Net: "tcp", Addr: &net.TCPAddr{IP: net.IPv4(10, 0, 0, 7), Port: 6379}, Err: err}
	}
	for name, tc := range map[string]struct {
		err  error
		want string
	}{
		"dns":             {&net.DNSError{Err: "no such host", Name: "private.internal", IsNotFound: true}, "redis host name could not be resolved"},
		"timeout":         {context.DeadlineExceeded, "connection to Redis timed out"},
		"refused":         {opErr(os.NewSyscallError("connect", syscall.ECONNREFUSED)), "redis refused the connection; check the host and port"},
		"refused windows": {opErr(os.NewSyscallError("connectex", wsaConnRefused)), "redis refused the connection; check the host and port"},
		"bad cert":        {&tls.CertificateVerificationError{Err: x509.UnknownAuthorityError{}}, "redis TLS certificate could not be verified"},
		"host cert":       {x509.HostnameError{Host: "private.internal"}, "redis TLS certificate could not be verified"},
		"not tls":         {tls.RecordHeaderError{Msg: "first record does not look like a TLS handshake"}, "redis did not answer with TLS; use redis:// for plain connections"},
		"eof":             {io.EOF, "redis closed the connection during the TLS handshake"},
		"other":           {errors.New("dial tcp 10.0.0.7:6379: something odd"), "could not connect to Redis"},
	} {
		t.Run(name, func(t *testing.T) {
			got := dialError(tc.err).Error()
			if got != tc.want {
				t.Fatalf("dialError = %q, want %q", got, tc.want)
			}
			if strings.Contains(got, "10.0.0.7") || strings.Contains(got, "private.internal") {
				t.Fatalf("dial error leaked an address: %q", got)
			}
		})
	}
}

func TestReasonFormatsSentences(t *testing.T) {
	for in, want := range map[string]string{
		"connection to Redis timed out":  "Connection to Redis timed out.",
		"Redis did not respond in time.": "Redis did not respond in time.",
		"  multi\n line   reason ":       "Multi line reason.",
	} {
		if got := Reason(errors.New(in)); got != want {
			t.Errorf("Reason(%q) = %q, want %q", in, got, want)
		}
	}
	if Reason(nil) != "" {
		t.Error("Reason(nil) should be empty")
	}
	if got := Reason(errors.New(strings.Repeat("é", 200))); len(got) > 182 || !strings.HasSuffix(got, ".") {
		t.Errorf("long reason was not bounded: %d bytes", len(got))
	}
}

// replyServer answers every connection's first command with reply (or closes
// it when reply is empty).
func replyServer(t *testing.T, reply string) net.Listener {
	t.Helper()
	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = conn.Close() }()
				r := bufio.NewReader(conn)
				header, err := r.ReadString('\n')
				if err != nil || !strings.HasPrefix(header, "*") {
					return
				}
				n, _ := strconv.Atoi(strings.TrimSpace(header[1:]))
				for range n * 2 {
					if _, err := r.ReadString('\n'); err != nil {
						return
					}
				}
				if reply != "" {
					_, _ = conn.Write([]byte(reply))
				}
			}()
		}
	}()
	return listener
}
