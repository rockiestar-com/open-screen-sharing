package rsd

import (
	"context"
	"fmt"
	"strconv"

	"github.com/google/uuid"
	"github.com/nekohasekai/open-screen-sharing/internal/coredevice/xpc"
	E "github.com/sagernet/sing/common/exceptions"
)

// pymobiledevice3 and go-ios use 58783 as the default RSD port
// (pymobiledevice3 9c33842, pymobiledevice3/remote/remote_service_discovery.py:38;
// go-ios ca2c56d, ios/rsd.go:119).
const Port = 58783

// idevice sends these MessagingProtocolVersion and RemoteXPCVersionFlags
// values in its RSD handshake (idevice 3854a5d, idevice/src/xpc/mod.rs:92-102).
const (
	messagingProtocolVersion = 7
	remoteXPCVersionFlags    = 0x0100000000000006
)

// remoted maps TlsPolicy 1, 2 and 3 to disabled, optional and required, and
// finishes a peer's handshake only after the peer answers the device's
// StartTls; macOS answers with YesNo false and TlsPolicy 1 (remoted, macOS
// 27.0.1 26A434, 0x1000076c4..0x100007728, 0x100008e2c..0x100009090,
// 0x1000194b4; observed on iPhone18,2, iOS 27.2 24B115).
const tlsPolicyDisabled = 1

type Peer struct {
	MessagingProtocolVersion uint64
	Properties               map[string]any
	Services                 map[string]uint16
}

func Handshake(ctx context.Context, conn *xpc.Conn, identity uuid.UUID) (*Peer, error) {
	err := conn.SendContext(ctx, map[string]any{
		"MessageType":              "Handshake",
		"MessagingProtocolVersion": uint64(messagingProtocolVersion),
		"UUID":                     identity,
		"Properties": map[string]any{
			"RemoteXPCVersionFlags":      uint64(remoteXPCVersionFlags),
			"SensitivePropertiesVisible": true,
		},
		"Services": map[string]any{},
	}, false)
	if err != nil {
		return nil, E.Cause(err, "RSD handshake")
	}
	for {
		message, err := conn.ReceiveMessage(ctx)
		if err != nil {
			return nil, E.Cause(err, "RSD handshake")
		}
		if message.Body["MessageType"] != "Handshake" {
			continue
		}
		return parsePeer(message.Body)
	}
}

func DeclineTLS(conn *xpc.Conn) error {
	err := conn.Send(map[string]any{
		"MessageType": "StartTls",
		"YesNo":       false,
		"TlsPolicy":   uint64(tlsPolicyDisabled),
	}, false)
	if err != nil {
		return E.Cause(err, "decline RSD TLS")
	}
	return nil
}

func parsePeer(body map[string]any) (*Peer, error) {
	peer := &Peer{Services: map[string]uint16{}}
	peer.MessagingProtocolVersion, _ = unsigned(body["MessagingProtocolVersion"])
	peer.Properties, _ = body["Properties"].(map[string]any)
	services, isDictionary := body["Services"].(map[string]any)
	if !isDictionary {
		return nil, E.New("RSD handshake: no services in ", fmt.Sprint(body))
	}
	for name, value := range services {
		entry, _ := value.(map[string]any)
		port, err := parsePort(entry["Port"])
		if err != nil {
			continue
		}
		peer.Services[name] = port
	}
	return peer, nil
}

func parsePort(value any) (uint16, error) {
	switch value := value.(type) {
	case string:
		port, err := strconv.ParseUint(value, 10, 16)
		if err != nil {
			return 0, E.New("bad port ", strconv.Quote(value))
		}
		return uint16(port), nil
	case int64, uint64:
		port, _ := unsigned(value)
		if port == 0 || port > 65535 {
			return 0, E.New("bad port ", port)
		}
		return uint16(port), nil
	}
	return 0, E.New("bad port ", fmt.Sprint(value))
}

func unsigned(value any) (uint64, bool) {
	switch value := value.(type) {
	case uint64:
		return value, true
	case int64:
		if value >= 0 {
			return uint64(value), true
		}
	}
	return 0, false
}
