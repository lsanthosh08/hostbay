package secrets

/*
#cgo CFLAGS: -x objective-c -fobjc-arc
#cgo LDFLAGS: -framework Foundation -framework LocalAuthentication
#include <stdlib.h>
#import <Foundation/Foundation.h>
#import <LocalAuthentication/LocalAuthentication.h>

enum { hbPassed = 1, hbCancelled, hbFailed, hbNotConfigured };

// hbVerify asks for Touch ID with fallback to the account password and
// blocks until the user answers or timeoutSec passes.
static int hbVerify(const char *reason, long timeoutSec) {
	LAContext *ctx = [[LAContext alloc] init];
	NSError *err = nil;
	if (![ctx canEvaluatePolicy:LAPolicyDeviceOwnerAuthentication error:&err]) {
		return hbNotConfigured;
	}
	__block int result = hbFailed;
	dispatch_semaphore_t done = dispatch_semaphore_create(0);
	[ctx evaluatePolicy:LAPolicyDeviceOwnerAuthentication
	    localizedReason:[NSString stringWithUTF8String:reason]
	              reply:^(BOOL ok, NSError *e) {
		if (ok) {
			result = hbPassed;
		} else if (e.code == LAErrorUserCancel || e.code == LAErrorSystemCancel || e.code == LAErrorAppCancel) {
			result = hbCancelled;
		} else if (e.code == LAErrorPasscodeNotSet) {
			result = hbNotConfigured;
		}
		dispatch_semaphore_signal(done);
	}];
	if (dispatch_semaphore_wait(done, dispatch_time(DISPATCH_TIME_NOW, timeoutSec * NSEC_PER_SEC)) != 0) {
		[ctx invalidate];
		return hbFailed;
	}
	return result;
}
*/
import "C"

import (
	"context"
	"unsafe"

	"hostbay/internal/store"
)

const notConfiguredMessage = "Set up Touch ID or a login password for your Mac account in System Settings to reveal or export saved secrets."

// NewVerifier returns the macOS verifier: Touch ID or the account password
// through LocalAuthentication. st, hwnd and ask are unused on macOS.
func NewVerifier(st *store.Store, hwnd uintptr, ask PasswordPrompt) Verifier {
	return touchIDVerifier{}
}

type touchIDVerifier struct{}

func (touchIDVerifier) Verify(ctx context.Context, reason string) Result {
	done := make(chan Result, 1)
	go func() {
		cr := C.CString(reason)
		defer C.free(unsafe.Pointer(cr))
		done <- Result(C.hbVerify(cr, 60)) // the Tauri app also denied after 60s
	}()
	select {
	case r := <-done:
		return r
	case <-ctx.Done():
		return Cancelled
	}
}
