package sseclient

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"time"

	"github.com/yabosh/logger"
)

var SSE_DATA_HEADER = []byte("data: ")
var SSE_HEADER_LEN = len(SSE_DATA_HEADER)
var SSE_DATA_TERMINATOR = []byte("\n\n")

const MIN_BUFFER_SIZE = 1024
const RECONNECT_DELAY = 15 * time.Second

const (
	Connected = iota
	Disconnected
	ErrorMsg
	DataMsg
)

// SSEClient manages a connection and the flow of information
// from an SSE endpoint
type SSEClient[T any] struct {
	url            string
	reconnect      bool
	bufferSize     int
	alertChannel   chan SSEMessage[T]
	reconnectDelay time.Duration
	resp           *http.Response
	ctx            context.Context
	cancelFunc     context.CancelFunc
	httpClient     *http.Client
}

// SSEMessage is a wrapper around any payload received from an
// SSE server.  This wrapper allows error messages to be
// passed to the SSE subscriber.
type SSEMessage[T any] struct {
	MessageType int
	Err         error
	Payload     T
}

// New creates a new, initialized instance of SSEClient
//
// Whenever a new message is received from the SSE endpoint a message
// will be sent to the alertChannel
func New[T any](url string, reconnect bool, alertChannel chan SSEMessage[T], bufferSize int) *SSEClient[T] {
	if bufferSize < MIN_BUFFER_SIZE {
		bufferSize = MIN_BUFFER_SIZE
	}

	obj := &SSEClient[T]{
		url:            url,
		reconnect:      reconnect,
		alertChannel:   alertChannel,
		bufferSize:     bufferSize,
		reconnectDelay: RECONNECT_DELAY,
	}

	return obj
}

// SetReconnectDelay sets the amount of time the client will wait before
// attempting to reconnect to an SSE server.  This value
// is only valid if the client was created  with reconnect=true
//
// This call is only necessary if the default value is not desired.
func (c *SSEClient[T]) SetReconnectDelay(delay time.Duration) {
	c.reconnectDelay = delay
}

// Connect will establish the initial connection to the SSE server.
func (c *SSEClient[T]) Connect() (err error) {
	return c.connect(false)
}

// Listen will connect to an SSE server and will listen for incoming events
func (c *SSEClient[T]) Listen() {
	if c == nil {
		panic("cannot listen on nil sse client")
	}

	if c.resp == nil {
		panic("call Connect() before listening to sse client")
	}

	// Process messages until the connection is interrupted in some way
	for {
		err := c.processMessages(c.resp)

		recon := os.IsTimeout(err)
		recon = recon || (err != nil && err.Error() == "EOF")
		recon = recon && c.reconnect

		if recon {
			c.sendDisconnect(err)

			// Create a new connection to the SSE server
			logger.Warn("attempting to reconnect to SSE server %s", c.url)
			c.connect(true)
			continue
		}

		if !recon && err.Error() == "EOF" {
			c.sendDisconnect(err)
			break
		}

		if err != nil {
			logger.Error("error with SSE connection to %s: %s", c.url, err.Error())
			payload := SSEMessage[T]{
				MessageType: ErrorMsg,
				Err:         err,
			}

			c.publish(payload)
		}
	}
}

func (c *SSEClient[T]) sendDisconnect(err error) {
	payload := SSEMessage[T]{
		MessageType: Disconnected,
		Err:         err,
	}

	c.publish(payload)

	// Cancel the existing connection before creating a new one
	c.resp.Body.Close()
	c.cancelFunc()
	c.httpClient.CloseIdleConnections()
}

// connect will  attempt to connect to the sse server
// and will, optionally, retry the connection
func (c *SSEClient[T]) connect(retry bool) (err error) {
	for {
		c.resp, err = c.connectSSE()

		if err != nil {
			payload := SSEMessage[T]{
				MessageType: ErrorMsg,
				Err:         err,
			}

			c.publish(payload)
		} else {
			payload := SSEMessage[T]{
				MessageType: Connected,
				Err:         nil,
			}

			c.publish(payload)
			return nil
		}

		if retry {
			time.Sleep(c.reconnectDelay)
			continue
		}

		return err
	}

}

// handleConnection will establish a connection and process
// messages until the connection is closed
// func (c *SSEClient[T]) handleConnection() error {
// 	resp, err := c.connectSSE()

// 	if err != nil {
// 		return err
// 	}

// 	return c.processMessages(resp)
// }

// processMessages will continuously read messages from the SSE channel and
// process them until the connection is closed or an error occurrs.
func (c *SSEClient[T]) processMessages(resp *http.Response) error {
	buffer := make([]byte, c.bufferSize)

	for {
		n, err := resp.Body.Read(buffer)

		if err != nil {
			return err
		}

		c.dumpBuffer(buffer, n)

		c.process(buffer[:n])
	}
}

func (c *SSEClient[T]) dumpBuffer(buffer []byte, length int) {
	if logger.GetLevel() < logger.TRACE {
		return
	}

	logger.Trace("Number of bytes read = %d", length)
	logger.Trace("Buffer:")
	logger.Trace(string(buffer[:length]))
}

// process will accept the contents of a single data message from an SSE
// endpoint, it will extract the relevant parts of that message,
// and then it will act on the contents accordingly.
func (c *SSEClient[T]) process(data []byte) error {
	var msg []byte

	for {
		// There may be multiple messages in a single SSE response.
		// Call extractMessage to retrieve the 'next' message
		// from the data buffer.
		msg, data = c.extractMessage(data)

		if msg == nil {
			return nil
		}

		c.handleMessage(msg)
	}
}

// handleMessage will accept a message from the SSE channel
// and will attempt process it as a JSON payload
func (c *SSEClient[T]) handleMessage(ssePacket []byte) {
	event, err := toJSON[T](ssePacket)

	if err != nil {
		payload := SSEMessage[T]{
			MessageType: ErrorMsg,
			Err:         err,
		}

		c.publish(payload)
	}

	payload := SSEMessage[T]{
		MessageType: DataMsg,
		Err:         nil,
		Payload:     event,
	}

	c.publish(payload)
}

// publish will send any received events to an alert
// channel.  This is a non-blocking call that will generate
// a warning if the channel is full.
func (c *SSEClient[T]) publish(event SSEMessage[T]) {
	select {
	case c.alertChannel <- event:
	default:
		logger.Warn("Received event from SSE but output channel was full")
	}
}

// extractMessage will extract a JSON string from an SSE packet
//
// SSE sends data using this format:
// data: <msg>\n\n
//
// Where: <msg> is the contents of the data being sent via SSE.
//
// This method will remove the `data: ` header and will read
// all bytes until it encounters a \n\n pattern.
//
// NOTE: There could be multiple messages contained in a single
// data packet.
//
// For instance, the following two messages could be  received from the SSE endpoint
// in the same request.  These messages would need to be parsed separately.
// =========
// data: {"event":"update_bays","source":"tba","site_id":"10","dob":"2024-02-11","version":56,"timestamp":"2024-02-07T17:26:19.185873911Z"}
//
// data: {"event":"update_activities","source":"tba","site_id":"10","dob":"2024-02-11","version":57,"timestamp":"2024-02-07T17:26:19.206915111Z"}
//
// =========
func (c *SSEClient[T]) extractMessage(data []byte) (msg []byte, buffer []byte) {
	if len(data) < SSE_HEADER_LEN {
		return nil, data
	}

	if !bytes.Equal(SSE_DATA_HEADER, data[:SSE_HEADER_LEN]) {
		// Not a valid SSE package
		return nil, data
	}

	// Remove the header
	data = data[SSE_HEADER_LEN:]

	// \n\n signals the end of the message so extract
	// all bytes until that pattern is found
	idx := bytes.Index(data, SSE_DATA_TERMINATOR)

	if idx < 1 {
		return nil, data
	}

	// Extract the contents of the message and then remove the
	// message from the data buffer
	msg = data[:idx]
	data = data[idx+len(SSE_DATA_TERMINATOR):]

	return msg, data
}

// toJSON will convert a byte array the generic type struct
func toJSON[T any](msg []byte) (obj T, err error) {
	var event T
	err = json.Unmarshal(msg, &event)
	return event, err
}

// connectSSE will establish a connection to an SSE endpoint
func (c *SSEClient[T]) connectSSE() (*http.Response, error) {
	req, err := c.createSSERequest()

	if err != nil {
		return nil, err
	}

	c.httpClient = &http.Client{}
	return c.httpClient.Do(req)
}

// createSSERequest will create an HTTP Request object that
// is compatible with an SSE endpoint.
func (c *SSEClient[T]) createSSERequest() (*http.Request, error) {
	// Add a context that can  be used to cancel any requests to the SSE server
	c.ctx, c.cancelFunc = context.WithCancel(context.Background())

	req, err := http.NewRequestWithContext(c.ctx, http.MethodGet, c.url, nil)

	if err != nil {
		return nil, err
	}

	c.addSSEHeaders(req)

	return req, nil
}

// addSSEHeaders will modify an HTTP Request so that it will properly
// handle SSE connections.
func (c *SSEClient[T]) addSSEHeaders(req *http.Request) {
	req.Header.Set("Cache-Control", "no-cache")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Connection", "keep-alive")
}
