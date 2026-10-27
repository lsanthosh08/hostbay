package secrets

import (
	"context"
	"runtime"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"

	"hostbay/internal/store"
)

const notConfiguredMessage = "Set up Windows Hello (a PIN, fingerprint or face) in Settings > Accounts > Sign-in options to reveal or export saved secrets."

// NewVerifier returns the Windows verifier: Windows Hello through
// UserConsentVerifier, with its prompt owned by the app window hwnd. st and
// ask are unused on Windows.
func NewVerifier(st *store.Store, hwnd uintptr, ask PasswordPrompt) Verifier {
	return helloVerifier{hwnd}
}

type helloVerifier struct{ hwnd uintptr }

var (
	combase                    = windows.NewLazySystemDLL("combase.dll")
	procRoInitialize           = combase.NewProc("RoInitialize")
	procRoUninitialize         = combase.NewProc("RoUninitialize")
	procRoGetActivationFactory = combase.NewProc("RoGetActivationFactory")
	procWindowsCreateString    = combase.NewProc("WindowsCreateString")
	procWindowsDeleteString    = combase.NewProc("WindowsDeleteString")

	iidUserConsentVerifierInterop = windows.GUID{Data1: 0x39e050c3, Data2: 0x4e74, Data3: 0x441a, Data4: [8]byte{0x8d, 0xc0, 0xb8, 0x11, 0x04, 0xdf, 0x94, 0x9c}}
	iidAsyncInfo                  = windows.GUID{Data1: 0x00000036, Data2: 0x0000, Data3: 0x0000, Data4: [8]byte{0xc0, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x46}}
	// IAsyncOperation<UserConsentVerificationResult>, the WinRT pinterface IID
	// of "pinterface({9fc2b0bb-e446-44e2-aa61-9cab8f636af2};enum(Windows.Security.Credentials.UI.UserConsentVerificationResult;i4))".
	iidAsyncOpResult = windows.GUID{Data1: 0xfd596ffd, Data2: 0x2318, Data3: 0x558f, Data4: [8]byte{0x9d, 0xbe, 0xd2, 0x1d, 0xf4, 0x37, 0x64, 0xa5}}
)

// Vtable slots. IUnknown is 0-2 and IInspectable 3-5.
const (
	slotQueryInterface = 0
	slotRelease        = 2
	// IUserConsentVerifierInterop
	slotRequestVerificationForWindowAsync = 6
	// IAsyncInfo
	slotStatus = 7
	slotCancel = 9
	// IAsyncOperation<T>
	slotGetResults = 8
)

const (
	rpcEChangedMode = 0x80010106
	asyncStarted    = 0
	asyncCompleted  = 1
	asyncCanceled   = 2
)

// UserConsentVerificationResult values.
const (
	ucvVerified             = 0
	ucvDeviceNotPresent     = 1
	ucvNotConfiguredForUser = 2
	ucvDisabledByPolicy     = 3
	ucvCanceled             = 6
)

type comObject struct{ vtbl *[16]uintptr }

// call invokes a vtable slot. uintptrescapes keeps out-parameters passed as
// &x on the heap, so a stack move cannot leave COM writing to a stale address.
//
//go:uintptrescapes
func (o *comObject) call(slot int, args ...uintptr) uintptr {
	r, _, _ := syscall.SyscallN(o.vtbl[slot], append([]uintptr{uintptr(unsafe.Pointer(o))}, args...)...)
	return r
}

func (o *comObject) release() { o.call(slotRelease) }

func failed(hr uintptr) bool { return int32(hr) < 0 }

func hstring(s string) (uintptr, bool) {
	u, err := windows.UTF16FromString(s)
	if err != nil {
		return 0, false
	}
	var h uintptr
	hr, _, _ := procWindowsCreateString.Call(uintptr(unsafe.Pointer(&u[0])), uintptr(len(u)-1), uintptr(unsafe.Pointer(&h)))
	return h, !failed(hr)
}

func (h helloVerifier) Verify(ctx context.Context, reason string) Result {
	if h.hwnd == 0 {
		return NotConfigured
	}
	done := make(chan Result, 1)
	go func() {
		// COM apartments belong to OS threads.
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		done <- h.verify(ctx, reason)
	}()
	return <-done
}

func (h helloVerifier) verify(ctx context.Context, reason string) Result {
	if procRoInitialize.Find() != nil {
		return NotConfigured // Windows before 8 has no WinRT
	}
	hr, _, _ := procRoInitialize.Call(1) // RO_INIT_MULTITHREADED
	if !failed(hr) {
		defer procRoUninitialize.Call()
	} else if uint32(hr) != rpcEChangedMode {
		return Failed
	}

	class, ok := hstring("Windows.Security.Credentials.UI.UserConsentVerifier")
	if !ok {
		return Failed
	}
	defer procWindowsDeleteString.Call(class)
	var interop *comObject
	hr, _, _ = procRoGetActivationFactory.Call(class, uintptr(unsafe.Pointer(&iidUserConsentVerifierInterop)), uintptr(unsafe.Pointer(&interop)))
	if failed(hr) || interop == nil {
		return NotConfigured
	}
	defer interop.release()

	msg, ok := hstring(reason)
	if !ok {
		return Failed
	}
	defer procWindowsDeleteString.Call(msg)
	var op *comObject
	hr = interop.call(slotRequestVerificationForWindowAsync, h.hwnd, msg, uintptr(unsafe.Pointer(&iidAsyncOpResult)), uintptr(unsafe.Pointer(&op)))
	if failed(hr) || op == nil {
		return Failed
	}
	defer op.release()
	var info *comObject
	if hr = op.call(slotQueryInterface, uintptr(unsafe.Pointer(&iidAsyncInfo)), uintptr(unsafe.Pointer(&info))); failed(hr) || info == nil {
		return Failed
	}
	defer info.release()

	// Polling the status avoids implementing a COM completion delegate.
	var status int32
	for {
		if hr = info.call(slotStatus, uintptr(unsafe.Pointer(&status))); failed(hr) {
			return Failed
		}
		if status != asyncStarted {
			break
		}
		select {
		case <-ctx.Done():
			info.call(slotCancel)
			return Cancelled
		case <-time.After(50 * time.Millisecond):
		}
	}
	switch status {
	case asyncCompleted:
	case asyncCanceled:
		return Cancelled
	default:
		return Failed
	}
	var res int32
	if hr = op.call(slotGetResults, uintptr(unsafe.Pointer(&res))); failed(hr) {
		return Failed
	}
	switch res {
	case ucvVerified:
		return Passed
	case ucvCanceled:
		return Cancelled
	case ucvDeviceNotPresent, ucvNotConfiguredForUser, ucvDisabledByPolicy:
		return NotConfigured
	default:
		return Failed
	}
}
