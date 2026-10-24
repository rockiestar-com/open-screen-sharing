//go:build darwin && !ios

// The usbmuxd protocol follows go-ios (ios/usbmuxconnection.go,
// ios/connect.go; MIT, Copyright (c) 2019 danielpaulus).

package lockdown

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"

	"github.com/sagernet/sing/common"
	E "github.com/sagernet/sing/common/exceptions"
	"howett.net/plist"
)

const (
	usbmuxdSocket = "/var/run/usbmuxd"
	// go-ios encodes the usbmuxd header as length, version 1, message type 8
	// (plist), then tag, all little endian (go-ios ca2c56d,
	// ios/usbmuxconnection.go:100-105, 163-166).
	usbmuxHeaderLength = 16
	usbmuxVersion      = 1
	usbmuxPlist        = 8
	usbmuxTag          = 1
)

type Device struct {
	DeviceID       int
	UDID           string
	ConnectionType string
}

func dialUSBMux(ctx context.Context) (net.Conn, error) {
	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "unix", usbmuxdSocket)
	if err != nil {
		return nil, E.Cause(err, "connect to usbmuxd")
	}
	return conn, nil
}

func usbmuxRequest(ctx context.Context, conn net.Conn, message map[string]any) (result map[string]any, resultErr error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer func() {
		if !stop() {
			conn.Close()
			result = nil
			resultErr = ctx.Err()
		}
	}()
	message["ClientVersionString"] = progName
	message["ProgName"] = progName
	message["kLibUSBMuxVersion"] = 3
	data, err := plist.Marshal(message, plist.XMLFormat)
	if err != nil {
		return nil, err
	}
	header := make([]byte, usbmuxHeaderLength)
	binary.LittleEndian.PutUint32(header, uint32(usbmuxHeaderLength+len(data)))
	binary.LittleEndian.PutUint32(header[4:], usbmuxVersion)
	binary.LittleEndian.PutUint32(header[8:], usbmuxPlist)
	binary.LittleEndian.PutUint32(header[12:], usbmuxTag)
	_, err = conn.Write(append(header, data...))
	if err == nil {
		_, err = io.ReadFull(conn, header)
	}
	if err != nil {
		return nil, contextError(ctx, E.Cause(err, "request usbmuxd"))
	}
	length := binary.LittleEndian.Uint32(header)
	if length < usbmuxHeaderLength || length > 16<<20 {
		return nil, E.New("usbmuxd: bad message length ", length, ", expected ", usbmuxHeaderLength, " to ", 16<<20)
	}
	data = make([]byte, length-usbmuxHeaderLength)
	_, err = io.ReadFull(conn, data)
	if err != nil {
		return nil, contextError(ctx, E.Cause(err, "read usbmuxd reply"))
	}
	var reply map[string]any
	_, err = plist.Unmarshal(data, &reply)
	if err != nil {
		return nil, E.Cause(err, "parse usbmuxd reply")
	}
	return reply, nil
}

func ListDevices(ctx context.Context) ([]Device, error) {
	conn, err := dialUSBMux(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	reply, err := usbmuxRequest(ctx, conn, map[string]any{"MessageType": "ListDevices"})
	if err != nil {
		return nil, err
	}
	list, _ := reply["DeviceList"].([]any)
	return common.Map(list, func(value any) Device {
		entry, _ := value.(map[string]any)
		properties, _ := entry["Properties"].(map[string]any)
		id, _ := entry["DeviceID"].(uint64)
		udid, _ := properties["SerialNumber"].(string)
		connectionType, _ := properties["ConnectionType"].(string)
		return Device{DeviceID: int(id), UDID: udid, ConnectionType: connectionType}
	}), nil
}

func findDevice(ctx context.Context, udid string) (Device, error) {
	devices, err := ListDevices(ctx)
	if err != nil {
		return Device{}, err
	}
	var found *Device
	for i, device := range devices {
		if device.UDID != udid {
			continue
		}
		if found == nil || device.ConnectionType == "USB" {
			found = &devices[i]
		}
	}
	if found == nil {
		return Device{}, E.New("usbmuxd: device ", udid, " is not connected")
	}
	return *found, nil
}

func OpenUSBMux(ctx context.Context, udid string) (*PairRecord, Dialer, error) {
	device, err := findDevice(ctx, udid)
	if err != nil {
		return nil, nil, err
	}
	conn, err := dialUSBMux(ctx)
	if err != nil {
		return nil, nil, err
	}
	defer conn.Close()
	reply, err := usbmuxRequest(ctx, conn, map[string]any{"MessageType": "ReadPairRecord", "PairRecordID": udid})
	if err != nil {
		return nil, nil, err
	}
	data, isData := reply["PairRecordData"].([]byte)
	if !isData {
		return nil, nil, E.New("usbmuxd: no pair record for ", udid, " (is the device trusted?): ", fmt.Sprint(reply))
	}
	record, err := ParsePairRecord(data)
	if err != nil {
		return nil, nil, err
	}
	return record, device.Dial, nil
}

func (d Device) Dial(ctx context.Context, port uint16) (net.Conn, error) {
	conn, err := dialUSBMux(ctx)
	if err != nil {
		return nil, err
	}
	reply, err := usbmuxRequest(ctx, conn, map[string]any{
		"MessageType": "Connect",
		"DeviceID":    d.DeviceID,
		// go-ios byte-swaps the port before encoding the usbmuxd Connect plist
		// (go-ios ca2c56d, ios/connect.go:45; ios/utils.go:101-105).
		"PortNumber": int(port>>8 | port<<8),
	})
	if err != nil {
		conn.Close()
		return nil, err
	}
	if number, _ := reply["Number"].(uint64); number != 0 {
		conn.Close()
		return nil, E.New("usbmuxd could not connect to port ", port, " (error ", number, ")")
	}
	return conn, nil
}

func contextError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return E.Extend(ctx.Err(), err)
	}
	return err
}
