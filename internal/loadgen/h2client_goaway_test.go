package loadgen

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/net/http2"
)

// TestH2Client_H2CPriorKnowledge asserts an http:// URL is driven as
// HTTP/2 without TLS (prior knowledge), not downgraded to HTTP/1.1.
func TestH2Client_H2CPriorKnowledge(t *testing.T) {
	var h2Hits, hits int64
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&hits, 1)
		if r.ProtoMajor == 2 {
			atomic.AddInt64(&h2Hits, 1)
		}
		w.WriteHeader(http.StatusOK)
	}))
	srv.Config.Protocols = new(http.Protocols)
	srv.Config.Protocols.SetHTTP1(true)
	srv.Config.Protocols.SetUnencryptedHTTP2(true)
	srv.Start()
	t.Cleanup(srv.Close)

	c, err := newH2Client(Config{
		URL:            srv.URL + "/",
		Method:         http.MethodGet,
		Protocol:       ProtocolHTTP2,
		ConnectionMode: KeepAlive,
		Timeout:        2 * time.Second,
	})
	if err != nil {
		t.Fatalf("newH2Client: %v", err)
	}
	for range 3 {
		if code, err := c.Do(context.Background()); err != nil || code != http.StatusOK {
			t.Fatalf("Do = %d, %v; want 200, nil", code, err)
		}
	}
	if atomic.LoadInt64(&h2Hits) != atomic.LoadInt64(&hits) {
		t.Errorf("h2c used for %d of %d requests, want all", h2Hits, hits)
	}
	if s := c.Stats(); s.ConnsOpened != 1 || s.StreamsIssued != 3 {
		t.Errorf("Stats = %+v, want 1 conn carrying 3 streams", s)
	}
}

// goAwayServer accepts h2c connections and answers the first request
// stream with GOAWAY (LastStreamID covering it, so the client may not
// retry) before closing the connection: a server shedding load.
func goAwayServer(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = conn.Close() }()
				preface := make([]byte, len(http2.ClientPreface))
				if _, err := io.ReadFull(conn, preface); err != nil {
					return
				}
				fr := http2.NewFramer(conn, conn)
				if err := fr.WriteSettings(); err != nil {
					return
				}
				for {
					f, err := fr.ReadFrame()
					if err != nil {
						return
					}
					if h, ok := f.(*http2.HeadersFrame); ok {
						_ = fr.WriteGoAway(h.StreamID, http2.ErrCodeEnhanceYourCalm, []byte("shedding"))
						return
					}
				}
			}()
		}
	}()
	return "http://" + ln.Addr().String() + "/"
}

// TestH2Client_CountsGoAway asserts a GOAWAY from the server is counted
// in Stats and classified as a server error.
func TestH2Client_CountsGoAway(t *testing.T) {
	c, err := newH2Client(Config{
		URL:            goAwayServer(t),
		Method:         http.MethodGet,
		Protocol:       ProtocolHTTP2,
		ConnectionMode: KeepAlive,
		Timeout:        2 * time.Second,
	})
	if err != nil {
		t.Fatalf("newH2Client: %v", err)
	}
	_, err = c.Do(context.Background())
	if err == nil || !strings.Contains(err.Error(), "GOAWAY") {
		t.Fatalf("Do error = %v, want a GOAWAY error", err)
	}
	if got := c.Stats().GoAwayCount; got != 1 {
		t.Errorf("GoAwayCount = %d, want 1", got)
	}
	if got := classify(err); got != ErrServer {
		t.Errorf("classify = %v, want ErrServer", got)
	}
}
