package server

import (
	"io"
	"net"
	"time"

	"github.com/dgrijalva/jwt-go"
	"github.com/fatih/color"
	protocol "github.com/gjbae1212/grpc-vpn/grpc/go"
	"github.com/gjbae1212/grpc-vpn/internal"
	"github.com/pkg/errors"
	"github.com/songgao/water/waterutil"
	"go.uber.org/atomic"
)

const (
	queueSizeForClientIn = 1000
)

type client struct {
	user     string                      // user
	originIP net.IP                      // user origin ip
	vpnIP    net.IP                      // user vpn ip
	jwt      *jwt.Token                  // user jwt token
	stream   protocol.VPN_ExchangeServer // stream
	loop     *atomic.Bool                // whether break loop or not
	exit     chan bool                   // exit

	out chan *protocol.IPPacket // out queue
	in  chan *protocol.IPPacket // in queue
}

// read packet
func (c *client) processReading() {
ReadLoop:
	for c.loop.Load() {
		packet, err := c.stream.Recv()
		if err == io.EOF {
			defaultLogger.Error(color.RedString("[ERR] %s (%s, %s) EOF",
				c.user, c.originIP.String(), c.vpnIP.String()))
			break ReadLoop
		}
		if err != nil {
			defaultLogger.Error(color.RedString("[ERR] %s (%s, %s) %s",
				c.user, c.originIP.String(), c.vpnIP.String(), err.Error()))
			break ReadLoop
		}

		// check error code
		switch packet.ErrorCode {
		case protocol.ErrorCode_EC_SUCCESS:
		default:
			// send unknown packet
			c.stream.Send(&protocol.IPPacket{ErrorCode: protocol.ErrorCode_EC_UNKNOWN})
			defaultLogger.Error(color.RedString("[ERR] %s (%s, %s) %s",
				c.user, c.originIP.String(), c.vpnIP.String(), internal.ErrorReceiveUnknownPacket.Error()))
			break ReadLoop
		}

		// check packet type
		switch packet.PacketType {
		case protocol.IPPacketType_IPPT_RAW:
		default:
			defaultLogger.Error(color.RedString("[ERR] %s (%s, %s) %s",
				c.user, c.originIP.String(), c.vpnIP.String(), internal.ErrorReceiveUnknownPacket.Error()))
			break ReadLoop
		}

		// check packet
		raw := packet.Packet1
		if raw == nil {
			defaultLogger.Error(color.RedString("[ERR] %s (%s, %s) %s",
				c.user, c.originIP.String(), c.vpnIP.String(), internal.ErrorReceiveUnknownPacket.Error()))
			break ReadLoop
		}

		// check source ip(equals vpn ip)
		srcIP := waterutil.IPv4Source(raw.Raw)
		if !srcIP.Equal(c.vpnIP) {
			defaultLogger.Error(color.RedString("[ERR] %s (%s, %s) %s(%s)",
				c.user, c.originIP.String(), c.vpnIP.String(), internal.ErrorReceiveUnknownPacket.Error(), srcIP))
			break ReadLoop
		}

		// out to server
		c.out <- packet
	}

	// flag off
	c.loop.Store(false)
	// writing exit
	c.exit <- true
}

// write packet
func (c *client) processWriting() {
	// make jwt checker
	jwtChecker := time.NewTicker(5 * time.Minute)

WriteLoop:
	for c.loop.Load() {
		select {
		case packet := <-c.in:
			if err := c.stream.Send(packet); err != nil {
				defaultLogger.Error(color.RedString("[ERR] %s (%s, %s) %s",
					c.user, c.originIP.String(), c.vpnIP.String(), err.Error()))
				break WriteLoop
			}
		case <-c.exit:
			defaultLogger.Error(color.RedString("[ERR] %s (%s, %s) exit signal",
				c.user, c.originIP.String(), c.vpnIP.String()))
			break WriteLoop
		case <-jwtChecker.C:
			// if JWT is expired, sending to error and break.
			if c.jwt.Claims.Valid() != nil {
				defaultLogger.Error(color.RedString("[ERR] %s (%s, %s) expired JWT",
					c.user, c.originIP.String(), c.vpnIP.String()))
				packet := &protocol.IPPacket{
					ErrorCode:  protocol.ErrorCode_EC_EXPIRED_JWT,
					PacketType: protocol.IPPacketType_IPPT_UNKNOWN,
				}
				if err := c.stream.Send(packet); err != nil {
					defaultLogger.Error(color.RedString("[ERR] %s (%s, %s) %s",
						c.user, c.originIP.String(), c.vpnIP.String(), err.Error()))
				}
				break WriteLoop
			}
		}
	}
	// stop jwt checker
	jwtChecker.Stop()
	// flag off
	c.loop.Store(false)
}

// hasVpnIP is to check whether to be assigned vpn ip in client or not.
func (c *client) hasVpnIP() bool {
	return c.vpnIP != nil
}

// newClient is to create new client.
func newClient(stream protocol.VPN_ExchangeServer, clientToServer chan *protocol.IPPacket) (*client, error) {
	if stream == nil {
		return nil, errors.Wrapf(internal.ErrorInvalidParams, "Method: newClient")
	}

	// extract params
	ctx := stream.Context()

	ip := ctx.Value(ipCtxName)
	if ip == nil {
		return nil, errors.Wrapf(internal.ErrorInvalidContext, "Method: newClient")
	}
	j := ctx.Value(jwtCtxName)
	if j == nil {
		return nil, errors.Wrapf(internal.ErrorInvalidContext, "Method: newClient")
	}

	c := &client{
		user:     j.(*jwt.Token).Claims.(*jwt.StandardClaims).Audience,
		originIP: ip.(net.IP),
		jwt:      j.(*jwt.Token),
		stream:   stream,
		loop:     atomic.NewBool(true),
		exit:     make(chan bool, 1),
		out:      clientToServer,                                      // vpn server  queue
		in:       make(chan *protocol.IPPacket, queueSizeForClientIn), // only exclusive client queue
	}

	return c, nil
}
