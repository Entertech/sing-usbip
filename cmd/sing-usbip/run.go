//go:build linux || (darwin && cgo) || windows

package main

import (
	"context"
	"encoding/json"
	"os"
	"os/signal"
	"syscall"

	"github.com/sagernet/sing-box/log"
	usbip "github.com/sagernet/sing-usbip"
	E "github.com/sagernet/sing/common/exceptions"

	"github.com/spf13/cobra"
)

var (
	deviceMatches     []string
	serverName        string
	captureMemoryPath string
	denyPath          string
	clientID          string
	clientHost        string
	clientUser        string
	noticeFile        string
)

// exitBlocked is the exit status of a client the bench owner blocked from
// every device it imports, so a supervisor can tell it from a crash.
const exitBlocked = 3

var runCommand = &cobra.Command{
	Use:   "run",
	Short: "Run the USB/IP server or client until interrupted",
	Long: "Run the USB/IP server or client until interrupted.\n\n" +
		"With --server, the matched devices are exported; at least one --device is\n" +
		"required. With --client, the matched devices are imported; without --device,\n" +
		"every device exported by the server is imported.",
	RunE: runService,
	Args: cobra.NoArgs,
}

func init() {
	runCommand.Flags().StringArrayVarP(&deviceMatches, "device", "d", nil, "device to export or import (repeatable)")
	runCommand.Flags().StringVar(&serverName, "name", "", "server name shown to clients (default: host name)")
	runCommand.Flags().StringVar(&captureMemoryPath, "capture-memory", "", "Windows server: file remembering the identities seen on port-only rules, so those devices are captured as they enumerate")
	runCommand.Flags().StringVar(&denyPath, "deny", "", "server: the bench owner's block list (JSON), read again when it changes")
	runCommand.Flags().StringVar(&clientID, "client-id", "", "client: a stable id for this client, shown by the server (display only)")
	runCommand.Flags().StringVar(&clientHost, "client-host", "", "client: host name shown by the server (default: host name)")
	runCommand.Flags().StringVar(&clientUser, "client-user", "", "client: user name shown by the server")
	runCommand.Flags().StringVar(&noticeFile, "notice-file", "", "client: when the bench owner blocks every imported device, write the notice (JSON) here before exiting with status 3")
	mainCommand.AddCommand(runCommand)
}

func runService(_ *cobra.Command, _ []string) error {
	devices, err := parseDeviceMatches(deviceMatches)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	switch {
	case serverAddress != "":
		return runServer(ctx, devices)
	case clientAddress != "":
		return runClient(ctx, devices)
	default:
		return E.New("one of --server or --client is required")
	}
}

func runServer(ctx context.Context, devices []usbip.DeviceMatch) error {
	if len(devices) == 0 {
		return E.New("at least one --device match is required")
	}
	listenAddress, err := parseListenAddress(serverAddress, usbip.DefaultPort)
	if err != nil {
		return err
	}
	name := serverName
	if name == "" {
		name, _ = os.Hostname()
	}
	service, err := usbip.NewServerService(ctx, usbip.ServerOptions{
		Logger:            log.StdLogger(),
		Name:              name,
		Devices:           devices,
		ListenAddress:     listenAddress,
		CaptureMemoryPath: captureMemoryPath,
		DenyPath:          denyPath,
	})
	if err != nil {
		return err
	}
	err = service.Start()
	if err != nil {
		_ = service.Close()
		return err
	}
	for _, match := range devices {
		log.Info("exporting ", formatDeviceMatch(match))
	}
	<-ctx.Done()
	return service.Close()
}

func runClient(ctx context.Context, devices []usbip.DeviceMatch) error {
	remoteAddress, err := parseServerAddress(clientAddress, usbip.DefaultPort)
	if err != nil {
		return err
	}
	host := clientHost
	if host == "" {
		host, _ = os.Hostname()
	}
	ctx, blocked := context.WithCancelCause(ctx)
	service, err := usbip.NewClientService(ctx, usbip.ClientOptions{
		Logger:        log.StdLogger(),
		ServerAddress: remoteAddress,
		Devices:       devices,
		Identity:      usbip.ClientIdentity{ClientID: clientID, Host: host, User: clientUser},
		OnNotice: func(notice usbip.ControlNotice, all bool) {
			if !all {
				return
			}
			if noticeFile != "" {
				raw, _ := json.Marshal(notice)
				if err := os.WriteFile(noticeFile, raw, 0o644); err != nil {
					log.Error("write notice: ", err)
				}
			}
			blocked(errBlocked)
		},
	})
	if err != nil {
		return err
	}
	err = service.Start()
	if err != nil {
		_ = service.Close()
		return err
	}
	if len(devices) == 0 {
		log.Info("importing every device exported by ", remoteAddress)
	} else {
		for _, match := range devices {
			log.Info("importing ", formatDeviceMatch(match))
		}
	}
	<-ctx.Done()
	_ = service.Close()
	if context.Cause(ctx) == errBlocked {
		return errBlocked
	}
	return nil
}

var errBlocked = exitError{exitBlocked, "the bench owner blocked every imported device"}

type exitError struct {
	code    int
	message string
}

func (e exitError) Error() string { return e.message }
