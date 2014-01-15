// Package goapenuts provides a connection to the Apple Push Notification service according to the docs at
// https://developer.apple.com/library/ios/documentation/NetworkingInternet/Conceptual/RemoteNotificationsPG/Chapters/CommunicatingWIthAPS.html
package goapenuts

import (
  "bytes"
  "crypto/tls"
  "encoding/binary"
  "errors"
  "fmt"
  "io"
  "net"
  "time"
)

// The APNS payload. Token should be a valid 32 byte device token. Payload is a json encoded
// payload object and must be less than or equal to 256 bytes in length. Expiration is a
// the number of seconds since the UNIX epoch that identifies when the APNS should discard
// the notification if it was not sent successfully or zero if the APNS should not store the 
// notification at all. Priority is 5 if the notification should be timed to conserve
// power or 10 if it should be sent immediately.
// Note that the notification identifier is not accessible. The connection uses it internally
// to track which notifications to resend on error.
//
// For simplified use, see NewPayload() and NewPersistentPayload().
//
// https://developer.apple.com/library/ios/documentation/NetworkingInternet/Conceptual/RemoteNotificationsPG/Chapters/ApplePushService.html
type Payload struct {
  Token      []byte
  Payload    []byte
  id         uint32
  Expiration uint32
  Priority   byte
}

// Represents an error reported by the APNS Feedback Service. Time is when the service
// determined that the token is no longer valid.
type Feedback struct {
  Time  uint32
  Token [32]byte
}

// Apple defined error codes
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

// Resend the message that triggered the error
var resendErrorCodes = map[uint8]bool{
  0:  true,
  10: true,
}

const GatewayAddress = "gateway.push.apple.com:2195"
const SandboxGatewayAddress = "gateway.sandbox.push.apple.com:2195"
const FeedbackAddress = "feedback.push.apple.com:2196"
const SandboxFeedbackAddress = "feedback.sandbox.push.apple.com:2196"

func frame(payload *Payload) []byte {
  frameBuffer := bytes.NewBuffer([]byte{})
  // command
  frameBuffer.WriteByte(1)
  binary.Write(frameBuffer, binary.BigEndian, uint16(len(payload.Token)))
  frameBuffer.Write(payload.Token)

  frameBuffer.WriteByte(2)
  binary.Write(frameBuffer, binary.BigEndian, uint16(len(payload.Payload)))
  frameBuffer.Write(payload.Payload)

  frameBuffer.WriteByte(3)
  binary.Write(frameBuffer, binary.BigEndian, uint16(4))
  binary.Write(frameBuffer, binary.BigEndian, payload.id)

  frameBuffer.WriteByte(4)
  binary.Write(frameBuffer, binary.BigEndian, uint16(4))
  binary.Write(frameBuffer, binary.BigEndian, payload.Expiration)

  frameBuffer.WriteByte(5)
  binary.Write(frameBuffer, binary.BigEndian, uint8(1))
  frameBuffer.WriteByte(payload.Priority)

  frameData := frameBuffer.Bytes()

  packageBuffer := bytes.NewBuffer([]byte{})
  packageBuffer.WriteByte(2)
  binary.Write(packageBuffer, binary.BigEndian, uint32(len(frameData)))
  packageBuffer.Write(frameData)
  return packageBuffer.Bytes()
}

// The connection to the APNS service. You may use multiple connections simultaneously
// for increased throughput. Connection is implemented as a goroutine that wraps a
// single tls.Conn to the APNS and listens for error responses when idle. If an error
// is received, the connection will automatically reconnect and attempt to resend
// recent payloads as needed.
type Connection struct {
  config          *tls.Config
  pushAddress     string
  feedbackAddress string
  conn            *tls.Conn
  push            chan *Payload
  stop            chan bool
  error           chan error
  payloadBuffer   []*Payload
  lastPayloadId   uint32
}

// A convenience method to create a new Payload that expires immediately and has
// immediate priority.
func NewPayload(token, payload []byte) *Payload {
  return &Payload{Token: token, Payload: payload, id: 0, Expiration: 0, Priority: 10}
}

// Like NewPayload() but allows you to specify a duration that will be used to
// calculate the expiration date sent to the APNS.
func NewPersistentPayload(token, payload []byte, expiration time.Duration) *Payload {
  return &Payload{Token: token, Payload: payload, id: 0, Expiration: uint32(time.Now().Add(expiration).Unix()), Priority: 10}
}

// Create a new APNS connection with the supplied Certificate. If sandbox is true,
// the sandbox gateway and feedback addresses will be used. bufferSize dictates
// the number of recent payloads to keep in memory should a resend be needed and must be
// greater than zero.
func NewConnection(cert tls.Certificate, sandbox bool, bufferSize uint32) *Connection {
  var pushAddress string
  var feedbackAddress string
  if sandbox {
    pushAddress = SandboxGatewayAddress
    feedbackAddress = SandboxFeedbackAddress
  } else {
    pushAddress = GatewayAddress
    feedbackAddress = FeedbackAddress
  }
  return &Connection{
    pushAddress:     pushAddress,
    feedbackAddress: feedbackAddress,
    config:          &tls.Config{Certificates: []tls.Certificate{cert}},
    push:            make(chan *Payload),
    error:           make(chan error),
    payloadBuffer:   make([]*Payload, bufferSize),
  }
}

func (c *Connection) nextPayloadId(i uint32) (n uint32) {
  n = i + 1
  if n == uint32(len(c.payloadBuffer)) {
    n = 0
  }
  return
}

func (c *Connection) copyPayloadBuffer(startId uint32, includeStart bool) (buffer []*Payload) {
  if startId >= uint32(len(c.payloadBuffer)) {
    buffer = []*Payload{}
    return
  }
  var outsize int
  if startId > c.lastPayloadId {
    outsize = len(c.payloadBuffer) - (int(startId) - int(c.lastPayloadId))
  } else {
    outsize = int(c.lastPayloadId) - int(startId)
  }
  if includeStart {
    outsize += 1
  } else {
    startId += 1
  }
  buffer = make([]*Payload, outsize)
  if outsize == 0 {
    return
  }
  for i, j := 0, startId; i < outsize; i, j = i+1, c.nextPayloadId(j) {
    buffer[i] = c.payloadBuffer[j]
  }
  return
}

func (c *Connection) sendError(err error) {
  if err == nil {
    return
  }
  select {
  case c.error <- err:
  default:
		// Errors are not being tracked, move on
  }
}

func (c *Connection) runPush() {
  defer c.conn.Close()
  response := [6]byte{}
  for {
    select {
    case <-c.stop:
      return
    case payload := <-c.push:
      payload.id = c.nextPayloadId(c.lastPayloadId)
      c.payloadBuffer[payload.id] = payload
      c.lastPayloadId = payload.id
      c.conn.Write(frame(payload))
    default:
      c.conn.SetReadDeadline(time.Now().Add(250 * time.Millisecond))
      n, err := c.conn.Read(response[:])
      if n > 0 {
        var command, status uint8
        var id uint32
        data := bytes.NewBuffer(response[:])
        binary.Read(data, binary.BigEndian, &command)
        binary.Read(data, binary.BigEndian, &status)
        binary.Read(data, binary.BigEndian, &id)
        c.sendError(errors.New(fmt.Sprintf("APNS Error %d: %s\n", status, ErrorMessages[status])))
        // Resend all payloads since the last successful push
        resendBuffer := c.copyPayloadBuffer(id, resendErrorCodes[status])
        c.sendError(c.connect())
        go c.runPush()
        for _, payload := range resendBuffer {
          if payload == nil {
          	// This may happen if the APNS returns an invalid ID
            break
          }
          c.Send(payload)
        }
        return
      }
      if err != nil {
        switch err.(type) {
        case net.Error:
          if err.(net.Error).Timeout() {
            continue
          }
        default:
        }

        c.sendError(c.connect())
        go c.runPush()
        return
      }
    }
  }
}

func (c *Connection) connect() (err error) {
  if c.conn != nil {
    c.conn.Close()
  }
  conn, err := net.Dial("tcp", c.pushAddress)
  if err != nil {
    return
  }
  c.conn = tls.Client(conn, c.config)
  err = c.conn.Handshake()
  return
}

// Connect to the APNS
func (c *Connection) Connect() (err error) {
  if c.conn != nil {
    return
  }
  c.stop = make(chan bool)
  if err = c.connect(); err != nil {
    return
  }
  go c.runPush()
  return
}

// Sends the specified payload to the APNS gateway. Calls block until the
// payload has been sent.
// This method is threadsafe.
func (c *Connection) Send(payload *Payload) {
  c.push <- payload
  return
}

// Disconnect from the APNS gateway and stop sending payloads.
// Any calls to Send() will block until a new connection is established.
func (c *Connection) Close() {
  if c.conn != nil {
    close(c.stop)
    c.conn = nil
  }
}

func (c *Connection) Connected() bool {
  return c.conn != nil
}

// Returns a channel that can be used to monitor errors from the APNS.
// Useful for logging purposes, no action should be necessary.
//
// Note: an unbuffered channel is used, so if you intend to track errors
// you should start listening before calling c.Connect()
func (c *Connection) Error() chan error {
  return c.error
}

// Check the APNS Feedback Service for invalid device tokens.
func (c *Connection) CheckFeedback() (feedback []*Feedback, err error) {
  feedback = make([]*Feedback, 0, 100)
  var conn net.Conn
  if conn, err = net.Dial("tcp", c.feedbackAddress); err != nil {
    return
  }
  feedbackConn := tls.Client(conn, c.config)
  defer feedbackConn.Close()
  if err = c.conn.Handshake(); err != nil {
    return
  }
  response := [38]byte{}
  for {
    if _, err = feedbackConn.Read(response[:]); err != nil {
      switch err.(type) {
      case net.Error:
        if err.(net.Error).Timeout() {
          err = nil
        }
      default:
        if err == io.EOF {
          err = nil
        }
      }
      return
    }
    f := &Feedback{Token: [32]byte{}}
    data := bytes.NewBuffer(response[:])
    binary.Read(data, binary.BigEndian, &f.Time)
    data.Next(2)
    data.Read(f.Token[:])
    feedback = append(feedback, f)
  }
}