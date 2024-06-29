# SSE Client for Go

sseclient is a library for receiving events from an HTTP Server Sent Events (SSE) endpoint. 

# Usage

```go
type MyMessage struct {
	Action string `json:"action"`
}

// OpenSSEChannel will establish a connection to an HTTP server that publishes an SSE endpoint.
// After establishing the connection it will create a goroutine to listen for any incoming messages
// on the connection.  The goroutine will send any messages received via SSE to the channel
// returned by the function.
func OpenSSEChannel(hostPath string, siteId string) (channel chan sseclient.SSEMessage[MyMessage], err error) {
	url := fmt.Sprintf("%s/sse", hostPath)

	// Create a channel to receive any incoming messages from the SSE link.
	// This channel should have enough capacity to ensure that messages won't be lost
	channel = make(chan sseclient.SSEMessage[MyMessage], 50)

	client := sseclient.New(url, true, channel, 1024)
	err = client.Connect()
	if err != nil {
		fmt.Println(err)
		return channel, err
	}

	go client.Listen()
	return channel, err
}
```