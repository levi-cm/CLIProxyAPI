package api

import (
	"bufio"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

type bodyLifecycleWriter struct {
	*httptest.ResponseRecorder
	conn      net.Conn
	deadlines []time.Time
}

func (w *bodyLifecycleWriter) SetReadDeadline(deadline time.Time) error {
	if !deadline.IsZero() {
		w.deadlines = append(w.deadlines, deadline)
	}
	return nil
}

func (w *bodyLifecycleWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return w.conn, bufio.NewReadWriter(bufio.NewReader(w.conn), bufio.NewWriter(w.conn)), nil
}

func TestRequestBodyCleanupDoesNotExpireAcceptedStreams(t *testing.T) {
	for _, hijack := range []bool{false, true} {
		t.Run(map[bool]string{false: "consumed-http-stream", true: "hijacked-connection"}[hijack], func(t *testing.T) {
			serverConn, clientConn := net.Pipe()
			t.Cleanup(func() { _ = serverConn.Close(); _ = clientConn.Close() })
			writer := &bodyLifecycleWriter{ResponseRecorder: httptest.NewRecorder(), conn: serverConn}
			server := &Server{cfg: &config.Config{}}
			router := gin.New()
			router.Use(server.maxRequestBodyBytesMiddleware())
			router.POST("/stream", func(c *gin.Context) {
				if hijack {
					if _, _, errHijack := c.Writer.Hijack(); errHijack != nil {
						t.Fatal(errHijack)
					}
					return
				}
				if _, errRead := io.Copy(io.Discard, c.Request.Body); errRead != nil {
					t.Fatal(errRead)
				}
				c.Header("Content-Type", "text/event-stream")
				if _, errWrite := c.Writer.WriteString("data: complete\n\n"); errWrite != nil {
					t.Fatal(errWrite)
				}
				c.Writer.Flush()
			})
			request := httptest.NewRequest(http.MethodPost, "/stream", strings.NewReader("request body"))
			wrapUnconsumedRequestBodyHandler(router).ServeHTTP(writer, request)
			if len(writer.deadlines) != 0 {
				t.Fatalf("accepted connection received a read deadline: %v", writer.deadlines)
			}
			if !hijack && writer.Body.String() != "data: complete\n\n" {
				t.Fatalf("stream payload changed: %q", writer.Body.String())
			}
		})
	}
}
