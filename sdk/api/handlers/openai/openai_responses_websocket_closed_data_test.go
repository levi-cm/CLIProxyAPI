package openai

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/interfaces"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/tidwall/gjson"
)

func TestForwardResponsesWebsocketDrainsLossBeforeClosedData(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, duplex := range []bool{false, true} {
		t.Run(fmt.Sprintf("duplex=%t", duplex), func(t *testing.T) {
			results := make(chan error, 32)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				conn, errUpgrade := responsesWebsocketUpgrader.Upgrade(w, r, nil)
				if errUpgrade != nil {
					results <- errUpgrade
					return
				}
				defer func() { _ = conn.Close() }()
				c, _ := gin.CreateTestContext(httptest.NewRecorder())
				c.Request = r
				data := make(chan []byte)
				errs := make(chan *interfaces.ErrorMessage, 1)
				loss := &interfaces.ErrorMessage{StatusCode: http.StatusBadGateway, Error: &coreexecutor.UpstreamWebsocketTurnError{Cause: errors.New("fixture upstream loss"), EventID: "turn-loss"}}
				errs <- loss
				close(errs)
				close(data)
				writer := newResponsesWebsocketWriter(conn)
				timeline := newInMemoryWebsocketTimelineLog()
				h := NewOpenAIResponsesAPIHandler(handlers.NewBaseAPIHandlers(nil, nil))
				var writeErr error
				_, _, _, errMsg, errForward := h.forwardResponsesWebsocket(c, writer, func(...interface{}) {}, data, errs, timeline, "closed-data", responsesWebsocketForwardOptions{
					duplexStream: func() bool { return duplex },
					suppressError: func(msg *interfaces.ErrorMessage) bool {
						writeErr = writeResponsesWebsocketUpstreamLoss(writer, timeline, msg)
						return true
					},
				})
				if errMsg != loss || errForward != nil || writeErr != nil {
					results <- fmt.Errorf("loss was bypassed: message=%v forward=%v write=%v", errMsg, errForward, writeErr)
					return
				}
				results <- conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"fixture.alive"}`))
			}))
			defer server.Close()
			// Both terminal channels are already ready. Correctness must not
			// depend on which case select chooses first.
			for i := 0; i < 32; i++ {
				conn, _, errDial := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), nil)
				if errDial != nil {
					t.Fatal(errDial)
				}
				t.Cleanup(func() { _ = conn.Close() })
				_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
				for _, want := range []string{"error", "fixture.alive"} {
					_, payload, errRead := conn.ReadMessage()
					if errRead != nil {
						_ = conn.Close()
						t.Fatalf("socket closed before %s: %v", want, errRead)
					}
					if got := gjson.GetBytes(payload, "type").String(); got != want {
						t.Fatalf("event=%q, want %q: %s", got, want, payload)
					}
					if want == "error" && (gjson.GetBytes(payload, "error.code").String() != "upstream_websocket_lost" || gjson.GetBytes(payload, "error.event_id").String() != "turn-loss") {
						t.Fatalf("uncorrelated loss: %s", payload)
					}
				}
				_ = conn.Close()
				if errResult := <-results; errResult != nil {
					t.Fatal(errResult)
				}
			}
		})
	}
}
