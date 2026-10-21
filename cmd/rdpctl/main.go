package main

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"math"
	"net"
	"os"
	"os/signal"
	"runtime"
	"runtime/pprof"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nekohasekai/open-screen-sharing/cmd/internal/ctl"
	"github.com/nekohasekai/open-screen-sharing/internal/bgra"
	"github.com/nekohasekai/open-screen-sharing/internal/rdp"
	E "github.com/sagernet/sing/common/exceptions"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	address := flag.String("address", "127.0.0.1:3389", "RDP server host:port")
	user := flag.String("user", "", "account name")
	domain := flag.String("domain", "", "account domain")
	credentials := flag.String("credentials", "", "private JSON file with host, port, username, password, domain")
	size := flag.String("size", "1280x800", "initial desktop size")
	desktopScale := flag.Int("desktop-scale", 100, "Windows desktop scale in percent (100–500)")
	fingerprint := flag.String("fingerprint", "", "expected SHA-256 certificate fingerprint; any certificate is accepted and reported when empty")
	timeout := flag.Duration("timeout", 30*time.Second, "connection timeout")
	duration := flag.Duration("duration", 10*time.Second, "receive duration when no actions are given")
	clipboard := flag.Bool("clipboard", true, "share text clipboard")
	audio := flag.Bool("audio", true, "play remote audio on this client")
	microphoneTone := flag.Float64("microphone-tone", 0, "redirect a microphone that records a sine tone of this frequency in Hz")
	cpuProfile := flag.String("cpu-profile", "", "write a Go CPU profile")
	audioOutput := flag.String("audio-out", "", "save up to 8 MiB of received PCM audio; format is reported in JSON")
	interactive := flag.Bool("interactive", false, "read additional actions from standard input")
	checkLogin := flag.Bool("check-login", false, "verify TLS and NLA without opening a desktop")
	debug := flag.Bool("debug", false, "log protocol details to standard error")
	graphicsCache := flag.String("graphics-cache", "", "file that keeps cached graphics between connections")
	flag.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: rdpctl [flags] [wait:DURATION | png:FILE | move:X,Y | click:X,Y[,left|right|middle] | scroll:X,Y,DY[,DX] | touch:began|moved|ended|cancelled,ID,X,Y | shared-clipboard:on|off | keysym:KEYSYM[,down|up] | mackey|mackeydown|mackeyup:CODE[,iso[,KEYSYM]] | pause | type:TEXT | paste:TEXT | clipboard-set:TEXT | clipboard-get | resize:WxH[,SCALE] | suppress:on|off | refresh | stats ...]")
		flag.PrintDefaults()
	}
	flag.Parse()
	width, height, err := ctl.ParseSize(*size)
	if err != nil {
		return err
	}
	h := &handler{start: time.Now(), tone: *microphoneTone}
	options := rdp.Options{Address: *address, Username: *user, Password: os.Getenv("RDPCTL_PASSWORD"), Domain: *domain, ClientName: "rdpctl", Width: width, Height: height, DesktopScaleFactor: *desktopScale, Clipboard: *clipboard, Audio: *audio, Microphone: *microphoneTone > 0, GraphicsCache: *graphicsCache, DialTimeout: *timeout}
	if *debug {
		options.Logger = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}
	options.VerifyCertificate = func(actual string) error {
		h.emit(map[string]any{"event": "certificate", "fingerprint": actual})
		if *fingerprint != "" && !strings.EqualFold(*fingerprint, actual) {
			return rdp.ErrCertificateChanged
		}
		return nil
	}
	if *credentials != "" {
		data, err := os.ReadFile(*credentials)
		if err != nil {
			return err
		}
		var c struct {
			Host                       string
			Port                       int
			Username, Password, Domain string
		}
		if err := json.Unmarshal(data, &c); err != nil {
			return E.Cause(err, "parse credentials file ", strconv.Quote(*credentials))
		}
		if c.Host != "" {
			if c.Port == 0 {
				c.Port = 3389
			}
			options.Address = net.JoinHostPort(c.Host, strconv.Itoa(c.Port))
		}
		options.Username, options.Password, options.Domain = c.Username, c.Password, c.Domain
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if *checkLogin {
		if err := rdp.CheckLogin(ctx, options); err != nil {
			return err
		}
		h.emit(map[string]any{"event": "login-verified", "milliseconds": time.Since(h.start).Seconds() * 1000})
		return nil
	}
	if *audioOutput != "" {
		var err error
		h.audioFile, err = os.OpenFile(*audioOutput, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
		if err != nil {
			return err
		}
		defer h.audioFile.Close()
	}
	if *cpuProfile != "" {
		file, err := os.Create(*cpuProfile)
		if err != nil {
			return err
		}
		defer file.Close()
		if err := pprof.StartCPUProfile(file); err != nil {
			return err
		}
		defer pprof.StopCPUProfile()
	}
	client, err := rdp.Dial(ctx, options, h)
	if err != nil {
		return err
	}
	defer client.Close()
	h.client.Store(client)
	h.emit(map[string]any{"event": "connected", "milliseconds": time.Since(h.start).Seconds() * 1000, "info": client.Info()})
	done := make(chan error, 1)
	go func() {
		done <- client.Run()
		close(done)
	}()
	actions := flag.Args()
	if len(actions) == 0 && !*interactive {
		actions = []string{"wait:" + duration.String()}
	}
	for _, action := range actions {
		if err := perform(ctx, client, h, action, done); err != nil {
			return E.Cause(err, "run ", action)
		}
	}
	if *interactive {
		lines := make(chan string)
		go func() {
			defer close(lines)
			scanner := bufio.NewScanner(os.Stdin)
			for scanner.Scan() {
				select {
				case lines <- scanner.Text():
				case <-ctx.Done():
					return
				}
			}
		}()
	interactiveLoop:
		for {
			select {
			case action, ok := <-lines:
				if !ok || action == "quit" {
					break interactiveLoop
				}
				if err := perform(ctx, client, h, action, done); err != nil {
					h.emit(map[string]any{"event": "action-error", "action": strings.SplitN(action, ":", 2)[0], "error": err.Error()})
				}
			case <-ctx.Done():
				return ctx.Err()
			case err := <-done:
				return err
			}
		}
	}
	h.stats(client)
	client.Close()
	select {
	case err := <-done:
		if err != nil && !E.IsMulti(err, net.ErrClosed, context.Canceled) && ctx.Err() == nil {
			return err
		}
	case <-ctx.Done():
		return ctx.Err()
	}
	return nil
}

func perform(ctx context.Context, c *rdp.Client, h *handler, action string, done <-chan error) error {
	name, value, _ := strings.Cut(action, ":")
	switch name {
	case "wait":
		d, err := time.ParseDuration(value)
		if err != nil {
			return err
		}
		timer := time.NewTimer(d)
		defer timer.Stop()
		select {
		case <-timer.C:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		case err := <-done:
			if err == nil {
				return E.New("server ended the session")
			}
			return err
		}
	case "png":
		frame := c.Snapshot()
		if frame == nil {
			return E.New("no framebuffer to save as ", strconv.Quote(value))
		}
		if err := ctl.WritePNG(value, ctl.Picture(frame)); err != nil {
			return err
		}
		h.emit(map[string]any{"event": "snapshot", "path": value})
		return nil
	case "move", "click":
		fields := strings.Split(value, ",")
		if len(fields) < 2 || len(fields) > 3 {
			return E.New(strconv.Quote(value), " is not X,Y[,BUTTON]")
		}
		n, err := ctl.ParseNumbers(strings.Join(fields[:2], ","), 2, strconv.Atoi)
		if err != nil {
			return err
		}
		buttonName := ""
		if len(fields) == 3 {
			buttonName = fields[2]
		}
		if name == "move" {
			return c.SendPointer(n[0], n[1], 0)
		}
		buttons := map[string]uint8{"": rdp.ButtonLeft, "left": rdp.ButtonLeft, "right": rdp.ButtonRight, "middle": rdp.ButtonMiddle}
		button, ok := buttons[buttonName]
		if !ok {
			return E.New("unknown button ", strconv.Quote(buttonName), "; it must be left, right or middle")
		}
		if err := c.SendPointer(n[0], n[1], button); err != nil {
			return err
		}
		return c.SendPointer(n[0], n[1], 0)
	case "scroll":
		n, err := ctl.ParseNumbers(value, 3, func(text string) (float64, error) { return strconv.ParseFloat(text, 64) })
		if err != nil {
			return err
		}
		deltaX := 0.0
		if len(n) > 3 {
			deltaX = n[3]
		}
		return c.SendScroll(int(n[0]), int(n[1]), deltaX, n[2])
	case "touch":
		phaseName, position, _ := strings.Cut(value, ",")
		phases := map[string]rdp.TouchPhase{"began": rdp.TouchBegan, "moved": rdp.TouchMoved, "ended": rdp.TouchEnded, "cancelled": rdp.TouchCancelled}
		phase, ok := phases[phaseName]
		if !ok {
			return E.New("unknown touch phase ", strconv.Quote(phaseName), "; it must be began, moved, ended or cancelled")
		}
		n, err := ctl.ParseNumbers(position, 3, strconv.Atoi)
		if err != nil {
			return err
		}
		return c.SendTouches([]rdp.Touch{{ID: int64(n[0]), Phase: phase, X: n[1], Y: n[2]}})
	case "pause":
		return c.SendKey(0xff13, true)
	case "keysym":
		keysymText, state, _ := strings.Cut(value, ",")
		keysym, err := strconv.ParseUint(keysymText, 0, 32)
		if err != nil {
			return err
		}
		if state != "up" {
			if err := c.SendKey(uint32(keysym), true); err != nil {
				return err
			}
		}
		if state != "down" {
			return c.SendKey(uint32(keysym), false)
		}
		return nil
	case "mackey", "mackeydown", "mackeyup":
		fields := strings.Split(value, ",")
		code, err := strconv.ParseUint(fields[0], 0, 16)
		if err != nil {
			return err
		}
		iso := len(fields) > 1 && fields[1] == "iso"
		var keysym uint64
		if len(fields) > 2 {
			keysym, err = strconv.ParseUint(fields[2], 0, 32)
			if err != nil {
				return err
			}
		}
		if name != "mackeyup" {
			if err := c.SendKeyCode(uint16(code), iso, uint32(keysym), true); err != nil {
				return err
			}
		}
		if name != "mackeydown" {
			return c.SendKeyCode(uint16(code), iso, uint32(keysym), false)
		}
		return nil
	case "suppress":
		if value != "on" && value != "off" {
			return E.New("suppress needs on|off, got ", strconv.Quote(value))
		}
		return c.SuppressOutput(value == "on")
	case "shared-clipboard":
		if value != "on" && value != "off" {
			return E.New("shared-clipboard needs on|off, got ", strconv.Quote(value))
		}
		return c.SetSharedClipboard(value == "on")
	case "type":
		return c.SendText(value)
	case "paste":
		return c.Paste(value)
	case "clipboard-set":
		return c.SetClipboard(value)
	case "clipboard-get":
		return c.RequestClipboard()
	case "resize":
		displaySize, scaleText, hasScale := strings.Cut(value, ",")
		width, height, err := ctl.ParseSize(displaySize)
		if err != nil {
			return err
		}
		scale := 0
		if hasScale {
			scale, err = strconv.Atoi(scaleText)
			if err != nil {
				return err
			}
		}
		return c.SetDisplay(width, height, scale)
	case "refresh":
		return c.RequestRefresh()
	case "stats":
		h.stats(c)
		return nil
	default:
		return E.New("unknown action ", strconv.Quote(name))
	}
}

type handler struct {
	client                                    atomic.Pointer[rdp.Client]
	tone                                      float64
	microphone                                chan struct{}
	audioFile                                 *os.File
	audioRecorded                             uint64
	access                                    sync.Mutex
	output                                    sync.Mutex
	start                                     time.Time
	first                                     time.Duration
	updates, pixels, audioPackets, audioBytes uint64
}

func (h *handler) emit(v any) {
	h.output.Lock()
	defer h.output.Unlock()
	_ = json.NewEncoder(os.Stdout).Encode(v)
}

func (h *handler) StateChanged(s rdp.State) {
	state := "connecting"
	if s == rdp.StateAuthenticating {
		state = "authenticating"
	}
	h.emit(map[string]any{"event": "state", "state": state})
}

func (h *handler) ResizeFramebuffer(width, height int) (*bgra.Image, error) {
	h.emit(map[string]any{"event": "resize", "width": width, "height": height})
	return bgra.New(width, height), nil
}

func (h *handler) FramebufferUpdated(update rdp.Update) {
	h.access.Lock()
	defer h.access.Unlock()
	if h.updates == 0 {
		h.first = time.Since(h.start)
		h.emit(map[string]any{"event": "first-frame", "milliseconds": h.first.Seconds() * 1000})
	}
	h.updates++
	h.pixels += uint64(update.Rectangle.Dx() * update.Rectangle.Dy())
}

func (h *handler) InfoChanged(info rdp.Info) {
	h.emit(map[string]any{"event": "info", "info": info})
}

func (h *handler) CursorChanged(*bgra.Cursor) {}

func (h *handler) CursorMoved(int, int) {}

func (h *handler) ClipboardReceived(text string) {
	h.emit(map[string]any{"event": "clipboard", "text": text})
}

func (h *handler) Bell() {
	h.emit(map[string]any{"event": "bell"})
}

func (h *handler) AudioFormatChanged(f rdp.AudioFormat) {
	h.emit(map[string]any{"event": "audio-format", "format": f})
}

func (h *handler) AudioPacketReceived(p []byte, _ uint64) {
	h.access.Lock()
	h.audioPackets++
	h.audioBytes += uint64(len(p))
	if h.audioFile != nil && h.audioRecorded < 8<<20 {
		n, err := h.audioFile.Write(p[:min(len(p), int((8<<20)-h.audioRecorded))])
		h.audioRecorded += uint64(n)
		if err != nil {
			h.emit(map[string]any{"event": "audio-write-error", "error": err.Error()})
		}
	}
	h.access.Unlock()
}

func (h *handler) MicrophoneOpened(f rdp.AudioFormat) {
	h.emit(map[string]any{"event": "microphone-open", "format": f})
	h.access.Lock()
	defer h.access.Unlock()
	if h.microphone != nil {
		close(h.microphone)
	}
	stop := make(chan struct{})
	h.microphone = stop
	go h.recordTone(f, stop)
}

func (h *handler) MicrophoneClosed() {
	h.emit(map[string]any{"event": "microphone-close"})
	h.access.Lock()
	defer h.access.Unlock()
	if h.microphone != nil {
		close(h.microphone)
		h.microphone = nil
	}
}

func (h *handler) recordTone(f rdp.AudioFormat, stop <-chan struct{}) {
	const interval = 20 * time.Millisecond
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	frames := f.SampleRate * int(interval/time.Millisecond) / 1000
	var position int
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
		}
		client := h.client.Load()
		if client == nil {
			continue
		}
		pcm := make([]byte, 0, frames*f.Channels*2)
		for range frames {
			sample := int16(math.Sin(2*math.Pi*h.tone*float64(position)/float64(f.SampleRate)) * 0.5 * math.MaxInt16)
			for range f.Channels {
				pcm = binary.LittleEndian.AppendUint16(pcm, uint16(sample))
			}
			position++
		}
		client.SendMicrophoneAudio(pcm)
	}
}

func (h *handler) stats(c *rdp.Client) {
	var memory runtime.MemStats
	runtime.ReadMemStats(&memory)
	info, statistics := c.Info(), c.Statistics()
	h.access.Lock()
	defer h.access.Unlock()
	elapsed := time.Since(h.start).Seconds()
	h.emit(map[string]any{"event": "stats", "unixMilliseconds": time.Now().UnixMilli(), "seconds": elapsed, "firstFrameMilliseconds": h.first.Seconds() * 1000, "updates": h.updates, "updatesPerSecond": float64(h.updates) / elapsed, "pixels": h.pixels, "audioPackets": h.audioPackets, "audioBytes": h.audioBytes, "audioRecorded": h.audioRecorded, "heapBytes": memory.HeapAlloc, "gcCycles": memory.NumGC, "info": info, "statistics": statistics})
}
