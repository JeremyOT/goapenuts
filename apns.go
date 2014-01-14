package goapenuts

// This package connects to the Apple Push Notification service according to the docs at
// https://developer.apple.com/library/ios/documentation/NetworkingInternet/Conceptual/RemoteNotificationsPG/Chapters/CommunicatingWIthAPS.html#//apple_ref/doc/uid/TP40008194-CH101-SW1

import (
	"bytes"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"time"
)

type ApnsPayload struct {
	Token      []byte
	Payload    []byte
	id         uint32
	Expiration uint32
	Priority   byte
}

// Apple define error codes
var ApnsErrorMessages = map[uint8]string{
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

//APNS Gateway addreses
var ApnsGatewayAddress = "gateway.push.apple.com:2195"
var ApnsSandboxGatewayAddress = "gateway.sandbox.push.apple.com:2195"

func frame(payload *ApnsPayload) []byte {
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

type ApnsConnection struct {
	config        *tls.Config
	address       string
	conn          *tls.Conn
	push          chan *ApnsPayload
	stop          chan byte
	error         chan error
	payloadBuffer []*ApnsPayload
	lastPayloadId uint32
}

func NewPayload(token, payload []byte) *ApnsPayload {
	return &ApnsPayload{Token: token, Payload: payload, id: 0, Expiration: 0, Priority: 10}
}

func NewPersistentPayload(token, payload []byte, expiration time.Duration) *ApnsPayload {
	return &ApnsPayload{Token: token, Payload: payload, id: 0, Expiration: uint32(time.Now().Add(expiration).Unix()), Priority: 10}
}

func NewConnection(sandbox bool, cert tls.Certificate) *ApnsConnection {
	var address string
	if sandbox {
		address = ApnsSandboxGatewayAddress
	} else {
		address = ApnsGatewayAddress
	}
	return &ApnsConnection{
		address:       address,
		config:        &tls.Config{Certificates: []tls.Certificate{cert}},
		stop:          make(chan byte),
		push:          make(chan *ApnsPayload),
		error:         make(chan error),
		payloadBuffer: make([]*ApnsPayload, 100),
	}
}

func (c *ApnsConnection) nextPayloadId(i uint32) (n uint32) {
	n = i + 1
	if n == uint32(len(c.payloadBuffer)) {
		n = 0
	}
	return
}

func (c *ApnsConnection) copyPayloadBuffer(startId uint32, includeStart bool) (buffer []*ApnsPayload) {
	if startId >= uint32(len(c.payloadBuffer)) {
		buffer = []*ApnsPayload{}
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
	buffer = make([]*ApnsPayload, outsize)
	if outsize == 0 {
		return
	}
	for i, j := 0, startId; i < outsize; i, j = i+1, c.nextPayloadId(j) {
		buffer[i] = c.payloadBuffer[j]
	}
	return
}

func (c *ApnsConnection) sendError(err error) {
	if err == nil {
		return
	}
	select {
	case c.error <- err:
	default:
	}
}

func (c *ApnsConnection) run() {
	currentConn := c.conn
	defer currentConn.Close()
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
				select {
				case c.error <- errors.New(fmt.Sprintf("APNS Error %d: %s\n", status, ApnsErrorMessages[status])):
				default:
				}
				// Resend all payloads since the last successful push
				resendBuffer := c.copyPayloadBuffer(id, resendErrorCodes[status])
				c.sendError(c.connect())
				go c.run()
				for _, payload := range resendBuffer {
					if payload == nil {
						break
					}
					c.Send(payload)
				}
				return
			}
			if err != nil {
				if err.(net.Error).Timeout() {
					continue
				}
				c.sendError(c.connect())
				go c.run()
				return
			}
		}
	}
}

func (c *ApnsConnection) connect() (err error) {
	if c.conn != nil {
		c.conn.Close()
	}
	conn, err := net.Dial("tcp", c.address)
	if err != nil {
		return
	}
	c.conn = tls.Client(conn, c.config)
	err = c.conn.Handshake()
	return
}

func (c *ApnsConnection) Connect() (err error) {
	err = c.connect()
	go c.run()
	return
}

func (c *ApnsConnection) Send(payload *ApnsPayload) {
	if c.conn == nil {
		c.Connect()
	}
	c.push <- payload
}

func (c *ApnsConnection) Stop() {
	if c.stop != nil {
		c.stop <- 1
		c.conn = nil
	}
}

func (c *ApnsConnection) IsRunning() bool {
	return c.conn != nil
}

func (c *ApnsConnection) LastError() (err error) {
	select {
	case err = <-c.error:
	default:
	}
	return
}

func (c *ApnsConnection) Error() chan error {
	return c.error
}
