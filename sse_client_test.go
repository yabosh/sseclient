package sseclient_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/yabosh/logger"
	"github.com/yabosh/sseclient"
)

func Test_read_from_sse_endpoint(t *testing.T) {
	// logger.SetLevel("TRACE")

	// Given an HTTP server that publishes an SSE endpoint
	sendMessageChannel := make(chan string, 10)
	done := make(chan bool, 1)
	svr := sseServer(sendMessageChannel, done)
	defer func() {
		done <- true
		svr.Close()
	}()

	// And an SSE client that is connected to the server
	sseChan, err := makeSSEChannel(svr.URL)

	if err != nil {
		assert.Fail(t, err.Error())
	}

	// And an event is generated on the server that should be sent to the client
	sendMessageChannel <- `{ "action": "run" }`

	// When messages are received from the SSE server

	// Then a 'connected' message should be received
	assert.Equal(t, sseclient.Connected, (<-sseChan).MessageType)

	// and a 'blank' message should be received
	assert.Equal(t, "", (<-sseChan).Payload.Action)

	// and a custom message should be received
	assert.Equal(t, "run", (<-sseChan).Payload.Action)
}

func Test_disconnect_message_when_sse_server_disconnects(t *testing.T) {
	// logger.SetLevel("TRACE")

	// Given an HTTP server that publishes an SSE endpoint
	sendMessageChannel := make(chan string, 10)
	done := make(chan bool, 1)
	svr := sseServer(sendMessageChannel, done)

	// And an SSE client that is connected to the server
	sseChan, err := makeSSEChannel(svr.URL)

	if err != nil {
		assert.Fail(t, err.Error())
	}

	<-sseChan // connected
	<-sseChan // blank message

	// When the server terminates
	done <- true

	// give the sse client some time to respond to the server terminating.  Not the most efficient way to do this but it's effective
	time.Sleep(500 * time.Millisecond)

	// Then a 'disconnected' message should be received
	assert.Equal(t, sseclient.Disconnected, (<-sseChan).MessageType)
}

/*
	Mock SSE server
*/

type MyMessage struct {
	Action string `json:"action"`
}

type ServerEventMessageResponse struct {
	Event     string    `json:"event"`
	Timestamp time.Time `json:"timestamp"`
}

// makeSSEChannel will establish a connection to an HTTP server that publishes an SSE endpoint.
// After establishing the connection it will create a goroutine to listen for any incoming messages
// on the connection.  The goroutine will send any messages received via SSE to the channel
// returned by the function.
func makeSSEChannel(hostPath string) (channel chan sseclient.SSEMessage[MyMessage], err error) {
	url := fmt.Sprintf("%s/sse", hostPath)

	// Create a channel to receive any incoming messages from the SSE link.
	// This channel should have enough capacity to ensure that messages won't be lost
	channel = make(chan sseclient.SSEMessage[MyMessage], 50)

	client := sseclient.New(url, false, channel, 1024)
	err = client.Connect()
	if err != nil {
		fmt.Println(err)
		return channel, err
	}

	go client.Listen()
	return channel, err
}

func sseServer(messagesToSend chan string, done chan bool) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flusher, _ := w.(http.Flusher)

		logger.Info("SSESVR: Connection request to SSE endpoint from remote at %s", r.RemoteAddr)

		// Add headers that make this call persistent and act as a Server Sent Events endpoint
		addSSEHeader(w)

		// The sub channel is the source of events that are sent to a connected SSE endpoint
		subchan := messagesToSend

		// Send an initial message indicating that the connection has been established
		conn_response := &ServerEventMessageResponse{
			Event:     "connected",
			Timestamp: time.Now(),
		}

		resp, _ := json.Marshal(conn_response)
		fmt.Fprintf(w, "data: %s\n\n", string(resp))
		logger.Info("SSESVR: data: %s\n\n", string(resp))
		flusher.Flush()
		logger.Info("SSESVR: sent 'connected'")

		// Push events until the client closes the SSE connection
		for {
			select {
			case event := <-subchan:
				_, err := fmt.Fprintf(w, "data: %s\n\n", event)
				logger.Info("SSESVR: data: %s\n\n", event)
				// A message could fail if the client port is no longer open because they closed the connection
				// If the client closed the connection then we need to unsub so subscriptions don't get too large
				if err != nil {
					logger.Error("SSESVR: error sending sse evt='%s' err='%s'", event, err)
					return
				}

				flusher.Flush()
				time.Sleep(1000)

			case <-done:
				logger.Info("SSESVR: is terminating due to done signal")
				return
			}
		}
	}))
}

// addSSEHeader will add the HTTP headers that are necessary to allow a client to recognize that
// the server supports SSE
func addSSEHeader(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("X-Accel-Buffering", "no")
}
