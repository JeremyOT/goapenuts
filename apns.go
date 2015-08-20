// Package goapenuts simplifies use of the Apple Push Notification service.
//
// For protocol details, see the docs available at https://developer.apple.com/library/ios/documentation/NetworkingInternet/Conceptual/RemoteNotificationsPG/Chapters/CommunicatingWIthAPS.html
package goapenuts

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"fmt"
	"io"
	"log"
	"net"
	"strconv"
	"time"
)

const (
	GatewayAddress                    = "gateway.push.apple.com"
	SandboxGatewayAddress             = "gateway.sandbox.push.apple.com"
	FeedbackAddress                   = "feedback.push.apple.com"
	SandboxFeedbackAddress            = "feedback.sandbox.push.apple.com"
	GatewayPort                       = 2195
	FeedbackPort                      = 2196
	NoError                StatusCode = 0
	ProcessingError        StatusCode = 1
	MissingDeviceToken     StatusCode = 2
	MissingTopic           StatusCode = 3
	MissingPayload         StatusCode = 4
	InvalidTokenSize       StatusCode = 5
	InvalidTopicSize       StatusCode = 6
	InvalidPayloadSize     StatusCode = 7
	InvalidToken           StatusCode = 8
	Shutdown               StatusCode = 10
	UnknownError           StatusCode = 255
	// frameOverhead is the constant size of all non-payload frame components
	frameOverhead = 3 + 32 + 3 + 3 + 4 + 3 + 4 + 3 + 1
	// packageOverhead is the size of the package header
	packageOverhead = 5
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

type StatusCode uint8

// Apple defined error codes
var ErrorMessages = map[StatusCode]string{
	NoError:            "No errors encountered",
	ProcessingError:    "Processing error",
	MissingDeviceToken: "Missing device token",
	MissingTopic:       "Missing topic",
	MissingPayload:     "Missing payload",
	InvalidTokenSize:   "Invalid token size",
	InvalidTopicSize:   "Invalid topic size",
	InvalidPayloadSize: "Invalid payload size",
	InvalidToken:       "Invalid token",
	Shutdown:           "Shutdown",
	UnknownError:       "None (unknown)",
}

// Resend the message that triggered the error
var resendStatusCodes = map[StatusCode]bool{
	NoError:  true,
	Shutdown: true,
}

func frame(payload *Payload) []byte {
	frameLength := frameOverhead + len(payload.Payload)
	packageBuffer := bytes.NewBuffer(make([]byte, 0, packageOverhead+frameLength))

	// enhanced notification frame
	packageBuffer.WriteByte(2)
	binary.Write(packageBuffer, binary.BigEndian, uint32(frameLength))

	// token
	packageBuffer.WriteByte(1)
	binary.Write(packageBuffer, binary.BigEndian, uint16(len(payload.Token)))
	packageBuffer.Write(payload.Token)
	// payload
	packageBuffer.WriteByte(2)
	binary.Write(packageBuffer, binary.BigEndian, uint16(len(payload.Payload)))
	packageBuffer.Write(payload.Payload)
	// payload id
	packageBuffer.WriteByte(3)
	binary.Write(packageBuffer, binary.BigEndian, uint16(4))
	binary.Write(packageBuffer, binary.BigEndian, payload.id)
	// expiration
	packageBuffer.WriteByte(4)
	binary.Write(packageBuffer, binary.BigEndian, uint16(4))
	binary.Write(packageBuffer, binary.BigEndian, payload.Expiration)
	// priority
	packageBuffer.WriteByte(5)
	binary.Write(packageBuffer, binary.BigEndian, uint16(1))
	packageBuffer.WriteByte(payload.Priority)

	return packageBuffer.Bytes()
}

// The connection to the APNS service. You may use multiple connections simultaneously
// for increased throughput. Connection is implemented as a goroutine that wraps a
// single tls.Conn to the APNS and listens for error responses when idle. If an error
// is received, the connection will automatically reconnect and attempt to resend
// recent payloads as needed.
type Connection struct {
	cert            tls.Certificate
	pushAddress     string
	feedbackAddress string
	conn            *tls.Conn
	push            chan *Payload
	stop            chan bool
	badToken        chan []byte
	payloadBuffer   []*Payload
	lastPayloadId   uint32
	keepAlivePeriod time.Duration
	noDelay         bool
	sandbox         bool
	rootCAs         *x509.CertPool
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
		cert:            cert,
		push:            make(chan *Payload),
		badToken:        make(chan []byte),
		payloadBuffer:   make([]*Payload, bufferSize),
		sandbox:         sandbox,
		noDelay:         true,
	}
}

// If greater than zero, keepalive messages will be sent on the connection with the given frequency.
// Must be called before Connect().
func (c *Connection) SetKeepAlivePeriod(keepAlivePeriod time.Duration) {
	c.keepAlivePeriod = keepAlivePeriod
}

// Sandbox returns true if this connection is in sandbox mode.
func (c *Connection) Sandbox() bool {
	return c.sandbox
}

// PushAddress returns the address of the configured push gateway.
func (c *Connection) PushAddress() string {
	return c.pushAddress
}

// FeedbackAddress returns the address of the configured feedback server.
func (c *Connection) FeedbackAddress() string {
	return c.feedbackAddress
}

// SetRootCAs sets the root certificate authorities that the client will use
// when connecting to the APNs. Pass nil (default) to use the host's root CA
// set. Must be called before Connect().
func (c *Connection) SetRootCAs(rootCAs *x509.CertPool) {
	c.rootCAs = rootCAs
}

// SetNoDelay is passed to the underlying TCP socket. Must be called before
// Connect. Default is true. See https://golang.org/pkg/net/#TCPConn.SetNoDelay
func (c *Connection) SetNoDelay(noDelay bool) {
	c.noDelay = noDelay
}

func (c *Connection) nextPayloadId(i uint32) (n uint32) {
	n = i + 1
	if n == uint32(len(c.payloadBuffer)) {
		n = 0
	}
	return
}

func (c *Connection) copyPayloadBuffer(startId uint32, includeStart bool) (buffer []*Payload, rejectedPayload *Payload) {
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
		rejectedPayload = c.payloadBuffer[startId]
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

func (c *Connection) sendBadToken(token []byte) {
	select {
	case c.badToken <- token:
	default:
		// Bad tokens are not being tracked, move on
	}
}

func (c *Connection) reconnect() (err error) {
	err = c.connect()
	if err != nil {
		log.Println("Connection error:", err)
		c.Close()
		return
	}
	go c.runPush()
	return
}

type apnsErrorResponse struct {
	hasResponse bool
	command     uint8
	status      StatusCode
	id          uint32
	error       error
}

func (c *Connection) readError(errorChannel chan apnsErrorResponse) {
	data := [6]byte{}
	response := apnsErrorResponse{}
	_, response.error = c.conn.Read(data[:])
	if response.error == nil {
		response.hasResponse = true
		buf := bytes.NewBuffer(data[:])
		binary.Read(buf, binary.BigEndian, &response.command)
		binary.Read(buf, binary.BigEndian, &response.status)
		binary.Read(buf, binary.BigEndian, &response.id)
		log.Printf("APNS Error %d: %s", response.status, ErrorMessages[response.status])
	}
	errorChannel <- response
}

func (c *Connection) runPush() {
	defer c.conn.Close()
	errorChannel := make(chan apnsErrorResponse)
	go c.readError(errorChannel)
	for {
		select {
		case <-c.stop:
			return
		case payload := <-c.push:
			payload.id = c.nextPayloadId(c.lastPayloadId)
			c.payloadBuffer[payload.id] = payload
			c.lastPayloadId = payload.id
			if _, err := c.conn.Write(frame(payload)); err != nil {
				log.Println("Error sending notification payload:", err)
				c.reconnect()
				return
			}
		case errorResponse := <-errorChannel:
			if errorResponse.hasResponse {
				log.Printf("APNS Error processing %d: %s", errorResponse.status, ErrorMessages[errorResponse.status])
				// Resend all payloads since the last successful push
				resendBuffer, rejectedPayload := c.copyPayloadBuffer(errorResponse.id, resendStatusCodes[errorResponse.status])
				if errorResponse.status == InvalidToken {
					c.sendBadToken(rejectedPayload.Token)
				}
				c.reconnect()
				for _, payload := range resendBuffer {
					if payload == nil {
						// This may happen if the APNS returns an invalid ID
						break
					}
					c.Send(payload)
				}
				return
			}
			if errorResponse.error != nil {
				log.Println("Error reading from APNS:", errorResponse.error)
				c.reconnect()
				return
			}
		}
	}
}

func (c *Connection) connect() (err error) {
	if c.conn != nil {
		c.conn.Close()
	}
	apnsURL := net.JoinHostPort(c.pushAddress, strconv.Itoa(GatewayPort))
	log.Println("Connecting to APNS.", apnsURL)
	conn, err := net.Dial("tcp", apnsURL)
	if err != nil {
		return
	}
	if c.keepAlivePeriod > 0 {
		if err = conn.(*net.TCPConn).SetKeepAlive(true); err != nil {
			return
		}
		if err = conn.(*net.TCPConn).SetKeepAlivePeriod(c.keepAlivePeriod); err != nil {
			return
		}
		if err = conn.(*net.TCPConn).SetNoDelay(c.noDelay); err != nil {
			return
		}
	}
	config := &tls.Config{Certificates: []tls.Certificate{c.cert}, ServerName: c.pushAddress, RootCAs: c.rootCAs}
	c.conn = tls.Client(conn, config)
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

// Returns a channel that can be used to monitor bad token responses from the APNS.
// Tokens sent to this channel should not be used again. Under normal operation
// invalid tokens will be returned by the CheckFeedback(), if tokens are sent
// through this channel they were likely send to the wrong service (e.g. sandbox sent
// to production).
//
// Note: an unbuffered channel is used, so if you intend to track bad tokens
// you should start listening before calling c.Connect()
func (c *Connection) BadToken() <-chan []byte {
	return c.badToken
}

// Check the APNS Feedback Service for invalid device tokens. These tokens represent
// uninstalled applications and should not be used again.
func (c *Connection) CheckFeedback() (feedback []*Feedback, err error) {
	feedback = make([]*Feedback, 0, 100)
	var conn net.Conn
	var read int
	if conn, err = net.Dial("tcp", fmt.Sprintf("%s:%d", c.feedbackAddress, FeedbackPort)); err != nil {
		return
	}
	config := &tls.Config{Certificates: []tls.Certificate{c.cert}, ServerName: c.feedbackAddress, RootCAs: c.rootCAs}
	feedbackConn := tls.Client(conn, config)
	defer feedbackConn.Close()
	if err = c.conn.Handshake(); err != nil {
		return
	}
	response := [38]byte{}
	for {
		feedbackConn.SetReadDeadline(time.Now().Add(30 * time.Second))
		if read, err = feedbackConn.Read(response[:]); err != nil {
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
		} else if read == 0 {
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
