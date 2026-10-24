//go:build darwin && !ios

package main

import (
	"context"
	"os"
	"slices"
	"strings"
	"text/tabwriter"

	"github.com/nekohasekai/open-screen-sharing/internal/coredevice/lockdown"
	"github.com/sagernet/sing/common"
	E "github.com/sagernet/sing/common/exceptions"

	"github.com/spf13/cobra"
)

var commandDevices = &cobra.Command{
	Use:   "devices",
	Short: "List the iPhones and iPads connected to this Mac",
	Args:  cobra.NoArgs,
	Run: func(cmd *cobra.Command, args []string) {
		err := printDevices(cmd.Context())
		if err != nil {
			fatal(err)
		}
	},
}

func init() {
	mainCommand.AddCommand(commandDevices)
}

type connectedDevice struct {
	lockdown.Device
	Name           string
	ProductType    string
	ProductVersion string
}

func printDevices(ctx context.Context) error {
	devices, err := connectedDevices(ctx)
	if err != nil {
		return err
	}
	if len(devices) == 0 {
		return nil
	}
	writer := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	writer.Write([]byte("NAME\tMODEL\tVERSION\tCONNECTION\tUDID\n"))
	for _, device := range devices {
		writer.Write([]byte(strings.Join([]string{device.Name, device.ProductType, device.ProductVersion, device.ConnectionType, device.UDID}, "\t") + "\n"))
	}
	return writer.Flush()
}

func connectedDevices(ctx context.Context) ([]connectedDevice, error) {
	listed, err := lockdown.ListDevices(ctx)
	if err != nil {
		return nil, err
	}
	var devices []connectedDevice
	for _, device := range listed {
		if device.UDID == "" {
			continue
		}
		index := slices.IndexFunc(devices, func(known connectedDevice) bool { return known.UDID == device.UDID })
		if index >= 0 {
			if device.ConnectionType == "USB" {
				devices[index].Device = device
			}
			continue
		}
		devices = append(devices, connectedDevice{Device: device})
	}
	for index := range devices {
		device := &devices[index]
		values, err := lockdown.QueryValues(ctx, device.Dial)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			logger.Warn("read the device's name", "udid", device.UDID, "error", err)
			continue
		}
		device.Name, _ = values["DeviceName"].(string)
		device.ProductType, _ = values["ProductType"].(string)
		device.ProductVersion, _ = values["ProductVersion"].(string)
	}
	return devices, nil
}

func selectDevice(ctx context.Context, udid string) (connectedDevice, error) {
	devices, err := connectedDevices(ctx)
	if err != nil {
		return connectedDevice{}, err
	}
	if udid != "" {
		index := slices.IndexFunc(devices, func(device connectedDevice) bool { return device.UDID == udid })
		if index < 0 {
			return connectedDevice{}, E.New("device ", udid, " is not connected")
		}
		return devices[index], nil
	}
	switch len(devices) {
	case 0:
		return connectedDevice{}, E.New("no iPhone or iPad is connected; connect one with a cable and trust this Mac on it")
	case 1:
		return devices[0], nil
	}
	return connectedDevice{}, E.New("more than one device is connected; choose one with --device: ", strings.Join(common.Map(devices, func(device connectedDevice) string {
		return device.UDID + " (" + device.Name + ")"
	}), ", "))
}
