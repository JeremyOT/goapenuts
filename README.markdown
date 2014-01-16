# Go (golang) support for sending notifications through the Apple Push Notification service

Package goapenuts simplifies use of the Apple Push Notification service.

For protocol details, see the docs available at
https://developer.apple.com/library/ios/documentation/NetworkingInternet/Conceptual/RemoteNotificationsPG/Chapters/CommunicatingWIthAPS.html

### Install

Install with:

  go get github.com/JeremyOT/goapenuts

### Example

```go
package main

import (
  "fmt"
  "time"
  "crypto/tls"
  "encoding/hex"
  apns "github.com/JeremyOT/goapenuts"
)

func main() {
  // Note key & cert must match the sandbox option
  apnsCert, _ := tls.LoadX509KeyPair("PATH_TO_CERT_FILE", "PATH_TO_KEY_FILE")
  sandbox := true
  // Keep the last 100 payloads in memory incase a resend is needed
  bufferSize := 100
  apnsConnection = apns.NewConnection(apnsCert, sandbox, bufferSize)
  apnsConnection.Connect()
  token, _ := hex.DecodeString("TOKEN_HEX_STRING")
  // See Apple docs for JSON structure
  // https://developer.apple.com/library/ios/documentation/NetworkingInternet/Conceptual/RemoteNotificationsPG/Chapters/ApplePushService.html
  payload := []byte("PAYLOAD_JSON_DATA")
  // Send the notification with an expiration one hour from now (Apple will retry this notification for up to an hour if it fails)
  apnsConnection.Send(apns.NewPersistentPayload(token, payload, time.Hour))
  wait := time.After(time.Second)
  select {
  // Check for an error and print it, no other action is necessary. goapenuts.Connection will automatically attempt to resend as needed.
  case err := <- apnsCapnsConnection:
    fmt.Println("APNS error:", err)
  }
  case <- wait:
    //Wait 1 second for the notification to send (happens much faster than this)
  }
}
```

### Notes

Connection.Send() is threadsafe and will block until the connection can send the notification. You may use multiple Connections for increased throughput.

## Usage

```go
const FeedbackAddress = "feedback.push.apple.com:2196"
```

```go
const GatewayAddress = "gateway.push.apple.com:2195"
```

```go
const SandboxFeedbackAddress = "feedback.sandbox.push.apple.com:2196"
```

```go
const SandboxGatewayAddress = "gateway.sandbox.push.apple.com:2195"
```

```go
var ErrorMessages = map[uint8]string{
	0:   "No errors encountered",
	1:   "Processing error",
	2:   "Missing device token",
	3:   "Missing topic",
	4:   "Missing payload",
	5:   "Invalid token size",
	6:   "Invalid topic size",
	7:   "Invalid payload size",
	8:   "Invalid token",
	10:  "Shutdown",
	255: "None (unknown)",
}
```
Apple defined error codes

#### type Connection

```go
type Connection struct {
}
```

The connection to the APNS service. You may use multiple connections
simultaneously for increased throughput. Connection is implemented as a
goroutine that wraps a single tls.Conn to the APNS and listens for error
responses when idle. If an error is received, the connection will automatically
reconnect and attempt to resend recent payloads as needed.

#### func  NewConnection

```go
func NewConnection(cert tls.Certificate, sandbox bool, bufferSize uint32) *Connection
```
Create a new APNS connection with the supplied Certificate. If sandbox is true,
the sandbox gateway and feedback addresses will be used. bufferSize dictates the
number of recent payloads to keep in memory should a resend be needed and must
be greater than zero.

#### func (*Connection) CheckFeedback

```go
func (c *Connection) CheckFeedback() (feedback []*Feedback, err error)
```
Check the APNS Feedback Service for invalid device tokens.

#### func (*Connection) Close

```go
func (c *Connection) Close()
```
Disconnect from the APNS gateway and stop sending payloads. Any calls to Send()
will block until a new connection is established.

#### func (*Connection) Connect

```go
func (c *Connection) Connect() (err error)
```
Connect to the APNS

#### func (*Connection) Connected

```go
func (c *Connection) Connected() bool
```

#### func (*Connection) Error

```go
func (c *Connection) Error() <-chan error
```
Returns a channel that can be used to monitor errors from the APNS. Useful for
logging purposes, no action should be necessary.

Note: an unbuffered channel is used, so if you intend to track errors you should
start listening before calling c.Connect()

#### func (*Connection) Send

```go
func (c *Connection) Send(payload *Payload)
```
Sends the specified payload to the APNS gateway. Calls block until the payload
has been sent. This method is threadsafe.

#### type Feedback

```go
type Feedback struct {
	Time  uint32
	Token [32]byte
}
```

Represents an error reported by the APNS Feedback Service. Time is when the
service determined that the token is no longer valid.

#### type Payload

```go
type Payload struct {
	Token   []byte
	Payload []byte

	Expiration uint32
	Priority   byte
}
```

The APNS payload. Token should be a valid 32 byte device token. Payload is a
json encoded payload object and must be less than or equal to 256 bytes in
length. Expiration is a the number of seconds since the UNIX epoch that
identifies when the APNS should discard the notification if it was not sent
successfully or zero if the APNS should not store the notification at all.
Priority is 5 if the notification should be timed to conserve power or 10 if it
should be sent immediately. Note that the notification identifier is not
accessible. The connection uses it internally to track which notifications to
resend on error.

For simplified use, see NewPayload() and NewPersistentPayload().

https://developer.apple.com/library/ios/documentation/NetworkingInternet/Conceptual/RemoteNotificationsPG/Chapters/ApplePushService.html

#### func  NewPayload

```go
func NewPayload(token, payload []byte) *Payload
```
A convenience method to create a new Payload that expires immediately and has
immediate priority.

#### func  NewPersistentPayload

```go
func NewPersistentPayload(token, payload []byte, expiration time.Duration) *Payload
```
Like NewPayload() but allows you to specify a duration that will be used to
calculate the expiration date sent to the APNS.
