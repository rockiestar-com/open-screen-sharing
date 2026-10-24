package rfb

import (
	"context"
	"crypto/des"
	"math/bits"
	"net"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/nekohasekai/open-screen-sharing/internal/gss"
	E "github.com/sagernet/sing/common/exceptions"
	F "github.com/sagernet/sing/common/format"
)

type SecurityType uint8

const (
	SecurityNone        SecurityType = 1
	SecurityVNCPassword SecurityType = 2
	SecurityAppleDH     SecurityType = 30
	SecurityAskObserve  SecurityType = 31
	SecurityAskControl  SecurityType = 32
	SecurityAppleRSA    SecurityType = 33
	SecurityApplePSK    SecurityType = 34
	SecurityKerberos    SecurityType = 35
	SecurityAppleSRP    SecurityType = 36
)

var securityNames = map[SecurityType]string{
	SecurityNone:        "none",
	SecurityVNCPassword: "vnc-password",
	SecurityAppleDH:     "apple-dh",
	SecurityAskObserve:  "ask-observe",
	SecurityAskControl:  "ask-control",
	SecurityAppleRSA:    "apple-rsa-srp",
	SecurityApplePSK:    "apple-psk",
	SecurityKerberos:    "kerberos",
	SecurityAppleSRP:    "apple-srp",
}

func (t SecurityType) String() string {
	name, found := securityNames[t]
	if found {
		return name + "(" + strconv.Itoa(int(t)) + ")"
	}
	return "security(" + strconv.Itoa(int(t)) + ")"
}

var (
	SecurityAccount        = []SecurityType{SecurityAppleRSA, SecurityAppleSRP, SecurityAppleDH, SecurityKerberos}
	SecurityAskPermission  = []SecurityType{SecurityAskControl, SecurityAskObserve}
	SecurityObserveRequest = []SecurityType{SecurityAskObserve, SecurityAskControl}
)

// AccountSecurityTypes orders account logins the way ScreenSharing does: it
// tries Kerberos first only for .local host names (ScreenSharing 776.27,
// macOS 27.0.1 26A434, UUID EC0B2C20-F816-3FCE-822D-F9403C09EB50,
// -[SSSessionView shouldPreferKerberosWithAuthTypes:wasBTMM:] 0x1cc0894bc,
// -[SSSessionView orderedArrayOfAuthTypesForMethod:preferKerberos:serverAllowedTypes:] 0x1cc089768).
func AccountSecurityTypes(host string) []SecurityType {
	if strings.HasSuffix(strings.ToLower(strings.TrimSuffix(host, ".")), ".local") {
		return []SecurityType{SecurityKerberos, SecurityAppleRSA, SecurityAppleSRP, SecurityAppleDH}
	}
	return SecurityAccount
}

type authenticator func(h *handshake) ([]byte, error)

var authenticators = map[SecurityType]authenticator{
	SecurityNone:        authenticateNone,
	SecurityVNCPassword: authenticateVNCPassword,
	SecurityAppleDH:     authenticateAppleDH,
	SecurityAskObserve:  authenticateAppleDH,
	SecurityAskControl:  authenticateAppleDH,
	SecurityAppleRSA:    authenticateAppleRSA,
	SecurityKerberos:    authenticateKerberos,
	SecurityAppleSRP:    authenticateAppleSRP,
}

var ErrUnsupportedServer = E.New("rfb: unsupported server")

var ErrUnverifiedServer = E.New("rfb: the server offers only account logins that do not verify its identity")

var ErrServerKeyChanged = E.New("rfb: the server key changed")

const kerberosTimeout = 5 * time.Second

func (h *handshake) chooseSecurityType(preferred []SecurityType) (SecurityType, error) {
	offered := h.info.SecurityTypes
	var unverified bool
	var kerberosErr error
	for _, candidate := range preferred {
		_, implemented := authenticators[candidate]
		if !implemented || !slices.Contains(offered, candidate) {
			continue
		}
		switch candidate {
		case SecurityAppleRSA:
			// ScreenSharing drops type 33 from an Apple server that offers
			// neither 35 nor 36 (ScreenSharing 776.27, macOS 27.0.1 26A434,
			// UUID EC0B2C20-F816-3FCE-822D-F9403C09EB50, 0x1cc08a038..0x1cc08a09c).
			if h.info.Dialect == DialectApple && !slices.Contains(offered, SecurityAppleSRP) && !slices.Contains(offered, SecurityKerberos) {
				continue
			}
		case SecurityAppleDH:
			if !h.options.AllowUnverifiedLogin {
				unverified = true
				continue
			}
		case SecurityKerberos:
			kdc, _, _ := net.SplitHostPort(h.options.Address)
			if address, isTCP := h.conn.RemoteAddr().(*net.TCPAddr); isTCP {
				kdc = netip.AddrPortFrom(address.AddrPort().Addr().Unmap(), 88).String()
			}
			ctx, cancel := context.WithTimeout(h.ctx, kerberosTimeout)
			var err error
			h.kerberos, err = gss.NewInitiator(ctx, h.options.Username, h.options.Password, kdc, "vnc")
			cancel()
			if err != nil {
				kerberosErr = err
				h.logger.Debug("skip kerberos", "error", err)
				continue
			}
		}
		return candidate, nil
	}
	offeredNames := strings.Join(F.MapToString(offered), ", ")
	if unverified {
		return 0, E.Extend(ErrUnverifiedServer, "it offers security types ", offeredNames)
	}
	err := E.Extend(ErrUnsupportedServer, "it offers security types ", offeredNames, "; none of ", strings.Join(F.MapToString(preferred), ", "))
	if kerberosErr != nil {
		return 0, E.Errors(err, kerberosErr)
	}
	return 0, err
}

func authenticateNone(*handshake) ([]byte, error) {
	return nil, nil
}

func authenticateVNCPassword(h *handshake) ([]byte, error) {
	challenge, err := h.stream.bytes(16)
	if err != nil {
		return nil, err
	}
	var key [8]byte
	copy(key[:], h.options.Password)
	for i := range key {
		key[i] = bits.Reverse8(key[i])
	}
	block, err := des.NewCipher(key[:])
	if err != nil {
		return nil, err
	}
	response := make([]byte, 16)
	block.Encrypt(response[:8], challenge[:8])
	block.Encrypt(response[8:], challenge[8:])
	_, err = h.conn.Write(response)
	return nil, err
}

func credentialBlock(username, password string) ([]byte, error) {
	// ScreenSharing's type-33 credential block randomizes 128 bytes, then copies
	// the username and password into separate 64-byte slots before encryption
	// (ScreenSharing 776.27, macOS 27.0.1 26A434, UUID EC0B2C20-F816-3FCE-822D-F9403C09EB50,
	// _RFBAuthenticateCore 0x1cc13faf4..0x1cc13fb44).
	if len(username) > 63 {
		return nil, E.New("rfb: user name of ", len(username), " bytes is over the 63-byte limit")
	}
	if len(password) > 63 {
		return nil, E.New("rfb: password is over the 63-byte limit")
	}
	if strings.IndexByte(username, 0) >= 0 || strings.IndexByte(password, 0) >= 0 {
		return nil, E.New("rfb: user name or password contains NUL")
	}
	block := randomBytes(128)
	copy(block, username)
	block[len(username)] = 0
	copy(block[64:], password)
	block[64+len(password)] = 0
	return block, nil
}
