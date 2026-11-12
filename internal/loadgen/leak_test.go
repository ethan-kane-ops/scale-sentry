package loadgen

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain fails the package's tests if any goroutine outlives them.
//
// fasthttp's cleaners are ignored: mCleaner and connsCleaner exit once a client
// has been idle for MaxIdleConnDuration (at most 10s), and tcpAddrsClean belongs
// to the process-wide default dialer. None grows per run.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m,
		goleak.IgnoreAnyFunction("github.com/valyala/fasthttp.(*Client).mCleaner"),
		goleak.IgnoreAnyFunction("github.com/valyala/fasthttp.(*HostClient).connsCleaner"),
		goleak.IgnoreAnyFunction("github.com/valyala/fasthttp.(*TCPDialer).tcpAddrsClean"),
	)
}
