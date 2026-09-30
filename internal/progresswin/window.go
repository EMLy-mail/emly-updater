package progresswin

import (
	"fmt"
	"io"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Options configure Run.
type Options struct {
	// Title is the window caption.
	Title string
	// IconPath is the executable whose first icon goes in the title bar and
	// taskbar. When it cannot be read (EMLy not installed yet), this
	// process's own icon is used instead.
	IconPath string

	// WaitService and WaitPID are for the self-update: the service that
	// drives the window is stopped by the very setup it launched, so its end
	// of the pipe closes before the installation has finished. When stdin
	// ends without a Close message, the window stays up until the service
	// named WaitService is running again under a process other than WaitPID
	// - the new build - or WaitTimeout has passed. Empty WaitService closes
	// at once.
	WaitService string
	WaitPID     uint32
	WaitTimeout time.Duration
}

var (
	user32   = windows.NewLazySystemDLL("user32.dll")
	gdi32    = windows.NewLazySystemDLL("gdi32.dll")
	shell32  = windows.NewLazySystemDLL("shell32.dll")
	comctl32 = windows.NewLazySystemDLL("comctl32.dll")
	kernel32 = windows.NewLazySystemDLL("kernel32.dll")

	procGetModuleHandle      = kernel32.NewProc("GetModuleHandleW")
	procRegisterClassEx      = user32.NewProc("RegisterClassExW")
	procCreateWindowEx       = user32.NewProc("CreateWindowExW")
	procDefWindowProc        = user32.NewProc("DefWindowProcW")
	procDestroyWindow        = user32.NewProc("DestroyWindow")
	procGetMessage           = user32.NewProc("GetMessageW")
	procTranslateMessage     = user32.NewProc("TranslateMessage")
	procDispatchMessage      = user32.NewProc("DispatchMessageW")
	procPostMessage          = user32.NewProc("PostMessageW")
	procSendMessage          = user32.NewProc("SendMessageW")
	procPostQuitMessage      = user32.NewProc("PostQuitMessage")
	procShowWindow           = user32.NewProc("ShowWindow")
	procSetWindowText        = user32.NewProc("SetWindowTextW")
	procGetWindowLongPtr     = user32.NewProc("GetWindowLongPtrW")
	procSetWindowLongPtr     = user32.NewProc("SetWindowLongPtrW")
	procGetSystemMenu        = user32.NewProc("GetSystemMenu")
	procEnableMenuItem       = user32.NewProc("EnableMenuItem")
	procAdjustWindowRectEx   = user32.NewProc("AdjustWindowRectEx")
	procSystemParametersInfo = user32.NewProc("SystemParametersInfoW")
	procGetDC                = user32.NewProc("GetDC")
	procReleaseDC            = user32.NewProc("ReleaseDC")
	procSetWindowPos         = user32.NewProc("SetWindowPos")
	procGetDeviceCaps        = gdi32.NewProc("GetDeviceCaps")
	procCreateFont           = gdi32.NewProc("CreateFontW")
	procCreateSolidBrush     = gdi32.NewProc("CreateSolidBrush")
	procDeleteObject         = gdi32.NewProc("DeleteObject")
	procSetTextColor         = gdi32.NewProc("SetTextColor")
	procSetBkColor           = gdi32.NewProc("SetBkColor")
	procExtractIconEx        = shell32.NewProc("ExtractIconExW")
	procInitCommonControlsEx = comctl32.NewProc("InitCommonControlsEx")
)

const (
	wsCaption   = 0x00C00000
	wsSysMenu   = 0x00080000
	wsChild     = 0x40000000
	wsVisible   = 0x10000000
	ssNoPrefix  = 0x00000080
	ssEndEllips = 0x00004000
	pbsSmooth   = 0x01
	pbsMarquee  = 0x08

	wmDestroy          = 0x0002
	wmClose            = 0x0010
	wmSetFont          = 0x0030
	wmSetIcon          = 0x0080
	wmCtlColorStatic   = 0x0138
	wmUser             = 0x0400
	wmAppUpdate        = 0x8000 + 1
	pbmSetPos          = wmUser + 2
	pbmSetRange32      = wmUser + 6
	pbmSetMarquee      = wmUser + 10
	iccProgressClass   = 0x00000020
	scClose            = 0xF060
	mfGrayed           = 0x00000001
	gwlStyle           = -16
	swShowNoActivate   = 4
	swpNoSize          = 0x0001
	swpNoMove          = 0x0002
	swpNoActivate      = 0x0010
	spiGetWorkArea     = 0x0030
	logPixelsY         = 90
	iconSmall          = 0
	iconBig            = 1
	defaultWaitTimeout = 3 * time.Minute

	// headingColor is the blue of Windows' own "please wait" dialogs,
	// RGB(0, 51, 153) as a COLORREF (0x00BBGGRR).
	headingColor = 0x00993300
	white        = 0x00FFFFFF
)

// gwlStyleIndex is GWL_STYLE as a variable: a negative constant cannot be
// converted to uintptr, a negative int is (sign-extended, as the API wants).
var gwlStyleIndex = gwlStyle

// HWND_TOPMOST (-1) and HWND_NOTOPMOST (-2), for the same reason.
var hwndTopmost, hwndNoTopmost = ^uintptr(0), ^uintptr(1)

type wndClassExW struct {
	cbSize        uint32
	style         uint32
	lpfnWndProc   uintptr
	cbClsExtra    int32
	cbWndExtra    int32
	hInstance     windows.Handle
	hIcon         windows.Handle
	hCursor       windows.Handle
	hbrBackground windows.Handle
	lpszMenuName  *uint16
	lpszClassName *uint16
	hIconSm       windows.Handle
}

type rect struct{ left, top, right, bottom int32 }

type msgT struct {
	hwnd    windows.Handle
	message uint32
	wParam  uintptr
	lParam  uintptr
	time    uint32
	pt      struct{ x, y int32 }
}

// window is the state the window procedure works on. Only the GUI thread
// touches the handles; pending/allowClose are the goroutine-safe hand-off.
type window struct {
	hwnd, heading, detail, bar windows.Handle
	brush                      windows.Handle
	shown                      bool
	marquee                    bool

	mu      sync.Mutex
	pending *Message

	allowClose atomic.Bool
}

// Run shows the window and drives it from in until in ends. It blocks, and
// must be the only GUI in the process.
func Run(opts Options, in io.Reader) error {
	// Every window call must come from the thread that created the window.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	w := &window{}
	if err := w.create(opts); err != nil {
		return err
	}
	defer procDeleteObject.Call(uintptr(w.brush))

	go func() {
		closed := readMessages(in, w.post)
		if !closed && opts.WaitService != "" {
			timeout := opts.WaitTimeout
			if timeout <= 0 {
				timeout = defaultWaitTimeout
			}
			waitForRestart(opts.WaitService, opts.WaitPID, timeout)
		}
		w.allowClose.Store(true)
		procPostMessage.Call(uintptr(w.hwnd), wmClose, 0, 0)
	}()

	var m msgT
	for {
		ret, _, _ := procGetMessage.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
		if int32(ret) <= 0 {
			return nil
		}
		procTranslateMessage.Call(uintptr(unsafe.Pointer(&m)))
		procDispatchMessage.Call(uintptr(unsafe.Pointer(&m)))
	}
}

// post hands m to the GUI thread; only the latest one is kept.
func (w *window) post(m Message) {
	w.mu.Lock()
	w.pending = &m
	w.mu.Unlock()
	procPostMessage.Call(uintptr(w.hwnd), wmAppUpdate, 0, 0)
}

func (w *window) create(opts Options) error {
	iccex := struct{ size, icc uint32 }{8, iccProgressClass}
	procInitCommonControlsEx.Call(uintptr(unsafe.Pointer(&iccex)))

	hInstance, _, _ := procGetModuleHandle.Call(0)
	small, large := loadIcons(opts.IconPath)

	brush, _, _ := procCreateSolidBrush.Call(white)
	w.brush = windows.Handle(brush)

	className := windows.StringToUTF16Ptr("AryxDAgentProgress")
	wc := wndClassExW{
		lpfnWndProc:   windows.NewCallback(w.wndProc),
		hInstance:     windows.Handle(hInstance),
		hIcon:         large,
		hIconSm:       small,
		hbrBackground: w.brush,
		lpszClassName: className,
	}
	wc.cbSize = uint32(unsafe.Sizeof(wc))
	if atom, _, err := procRegisterClassEx.Call(uintptr(unsafe.Pointer(&wc))); atom == 0 {
		return fmt.Errorf("RegisterClassEx: %w", err)
	}

	// Layout in 96-DPI pixels, scaled to the screen's DPI (the manifest
	// declares the process DPI-aware, so nothing else scales it for us).
	dpi := screenDPI()
	px := func(v int32) int32 { return v * dpi / 96 }
	const (
		clientW = 440
		clientH = 128
		margin  = 20
	)

	// Caption and system menu only: no minimize/maximize, no resizing
	// border, and the close button is disabled below - exactly Windows' own
	// "please wait while the features are downloaded" dialog, but with an icon.
	style := uint32(wsCaption | wsSysMenu)
	r := rect{0, 0, px(clientW), px(clientH)}
	procAdjustWindowRectEx.Call(uintptr(unsafe.Pointer(&r)), uintptr(style), 0, 0)
	width, height := r.right-r.left, r.bottom-r.top
	x, y := centerOnWorkArea(width, height)

	title := windows.StringToUTF16Ptr(opts.Title)
	hwnd, _, err := procCreateWindowEx.Call(0,
		uintptr(unsafe.Pointer(className)), uintptr(unsafe.Pointer(title)),
		uintptr(style), uintptr(x), uintptr(y), uintptr(width), uintptr(height),
		0, 0, hInstance, 0)
	if hwnd == 0 {
		return fmt.Errorf("CreateWindowEx: %w", err)
	}
	w.hwnd = windows.Handle(hwnd)
	if large != 0 {
		procSendMessage.Call(hwnd, wmSetIcon, iconBig, uintptr(large))
	}
	if small != 0 {
		procSendMessage.Call(hwnd, wmSetIcon, iconSmall, uintptr(small))
	}

	sysMenu, _, _ := procGetSystemMenu.Call(hwnd, 0)
	procEnableMenuItem.Call(sysMenu, scClose, mfGrayed)

	child := func(class string, style uint32, x, y, w, h int32) windows.Handle {
		c, _, _ := procCreateWindowEx.Call(0,
			uintptr(unsafe.Pointer(windows.StringToUTF16Ptr(class))), 0,
			uintptr(wsChild|wsVisible|style),
			uintptr(px(x)), uintptr(px(y)), uintptr(px(w)), uintptr(px(h)),
			hwnd, 0, hInstance, 0)
		return windows.Handle(c)
	}
	inner := int32(clientW - 2*margin)
	w.heading = child("STATIC", ssNoPrefix|ssEndEllips, margin, 18, inner, 26)
	w.detail = child("STATIC", ssNoPrefix|ssEndEllips, margin, 50, inner, 20)
	w.bar = child("msctls_progress32", pbsSmooth, margin, 86, inner, 22)

	procSendMessage.Call(uintptr(w.heading), wmSetFont, font(12, dpi), 1)
	procSendMessage.Call(uintptr(w.detail), wmSetFont, font(9, dpi), 1)
	procSendMessage.Call(uintptr(w.bar), pbmSetRange32, 0, 100)
	return nil
}

func (w *window) wndProc(hwnd windows.Handle, msg uint32, wParam, lParam uintptr) uintptr {
	switch msg {
	case wmAppUpdate:
		w.mu.Lock()
		m := w.pending
		w.pending = nil
		w.mu.Unlock()
		if m != nil {
			w.apply(*m)
		}
		return 0
	case wmCtlColorStatic:
		hdc := wParam
		color := uintptr(0)
		if windows.Handle(lParam) == w.heading {
			color = headingColor
		}
		procSetTextColor.Call(hdc, color)
		procSetBkColor.Call(hdc, white)
		return uintptr(w.brush)
	case wmClose:
		// Alt+F4 and the (disabled) close button end up here too: only the
		// service, through the pipe, decides when the window goes.
		if !w.allowClose.Load() {
			return 0
		}
		procDestroyWindow.Call(uintptr(hwnd))
		return 0
	case wmDestroy:
		procPostQuitMessage.Call(0)
		return 0
	}
	ret, _, _ := procDefWindowProc.Call(uintptr(hwnd), uintptr(msg), wParam, lParam)
	return ret
}

func (w *window) apply(m Message) {
	setText(w.heading, m.Heading)
	setText(w.detail, m.Detail)

	marquee := m.Percent < 0 || m.Percent > 100
	if marquee != w.marquee {
		style, _, _ := procGetWindowLongPtr.Call(uintptr(w.bar), uintptr(gwlStyleIndex))
		if marquee {
			style |= pbsMarquee
		} else {
			style &^= pbsMarquee
		}
		procSetWindowLongPtr.Call(uintptr(w.bar), uintptr(gwlStyleIndex), style)
		on := uintptr(0)
		if marquee {
			on = 1
		}
		procSendMessage.Call(uintptr(w.bar), pbmSetMarquee, on, 30)
		w.marquee = marquee
	}
	if !marquee {
		procSendMessage.Call(uintptr(w.bar), pbmSetPos, uintptr(m.Percent), 0)
	}

	// Shown on the first message, not at creation, so it never appears
	// empty. A process started by a service is not allowed to take the
	// foreground, and a window it shows lands behind whatever the user is
	// working in - where nobody would see it. Passing through the topmost
	// band lifts it to the top of the z-order without activating it: it is
	// visible, and the keyboard focus stays where the user left it.
	if !w.shown {
		procShowWindow.Call(uintptr(w.hwnd), swShowNoActivate)
		for _, after := range []uintptr{hwndTopmost, hwndNoTopmost} {
			procSetWindowPos.Call(uintptr(w.hwnd), after, 0, 0, 0, 0, swpNoMove|swpNoSize|swpNoActivate)
		}
		w.shown = true
	}
}

func setText(h windows.Handle, s string) {
	p, err := windows.UTF16PtrFromString(s)
	if err != nil {
		return
	}
	procSetWindowText.Call(uintptr(h), uintptr(unsafe.Pointer(p)))
}

func screenDPI() int32 {
	hdc, _, _ := procGetDC.Call(0)
	if hdc == 0 {
		return 96
	}
	defer procReleaseDC.Call(0, hdc)
	dpi, _, _ := procGetDeviceCaps.Call(hdc, logPixelsY)
	if dpi == 0 {
		return 96
	}
	return int32(dpi)
}

// font creates a Segoe UI font of the given point size. It lives as long as
// the process: the window is the process's only reason to exist.
func font(points, dpi int32) uintptr {
	h, _, _ := procCreateFont.Call(
		uintptr(-(points*dpi+36)/72), // negative: character height, rounded
		0, 0, 0,
		400,     // FW_NORMAL
		0, 0, 0, // italic, underline, strikeout
		1, // DEFAULT_CHARSET
		0, 0,
		5, // CLEARTYPE_QUALITY
		0,
		uintptr(unsafe.Pointer(windows.StringToUTF16Ptr("Segoe UI"))))
	return h
}

func centerOnWorkArea(width, height int32) (int32, int32) {
	var wa rect
	if ret, _, _ := procSystemParametersInfo.Call(spiGetWorkArea, 0, uintptr(unsafe.Pointer(&wa)), 0); ret == 0 {
		return 100, 100
	}
	return wa.left + (wa.right-wa.left-width)/2, wa.top + (wa.bottom-wa.top-height)/2
}

// loadIcons returns the small and large icons of path, falling back to this
// executable's own.
func loadIcons(path string) (small, large windows.Handle) {
	if path != "" {
		if small, large = extractIcons(path); large != 0 {
			return small, large
		}
	}
	if self, err := os.Executable(); err == nil {
		return extractIcons(self)
	}
	return 0, 0
}

func extractIcons(path string) (small, large windows.Handle) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, 0
	}
	ret, _, _ := procExtractIconEx.Call(uintptr(unsafe.Pointer(p)), 0,
		uintptr(unsafe.Pointer(&large)), uintptr(unsafe.Pointer(&small)), 1)
	if ret == 0 || ret == ^uintptr(0) {
		return 0, 0
	}
	return small, large
}

// waitForRestart returns once the service is running under a process other
// than oldPID, or after timeout. Querying a service's status needs no
// privileges, so this works from the user's session.
func waitForRestart(name string, oldPID uint32, timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if pid, running := servicePID(name); running && pid != oldPID {
			return
		}
		time.Sleep(time.Second)
	}
}

func servicePID(name string) (uint32, bool) {
	m, err := windows.OpenSCManager(nil, nil, windows.SC_MANAGER_CONNECT)
	if err != nil {
		return 0, false
	}
	defer windows.CloseServiceHandle(m)
	s, err := windows.OpenService(m, windows.StringToUTF16Ptr(name), windows.SERVICE_QUERY_STATUS)
	if err != nil {
		return 0, false
	}
	defer windows.CloseServiceHandle(s)
	var st windows.SERVICE_STATUS_PROCESS
	var needed uint32
	if err := windows.QueryServiceStatusEx(s, windows.SC_STATUS_PROCESS_INFO,
		(*byte)(unsafe.Pointer(&st)), uint32(unsafe.Sizeof(st)), &needed); err != nil {
		return 0, false
	}
	return st.ProcessId, st.CurrentState == windows.SERVICE_RUNNING
}
