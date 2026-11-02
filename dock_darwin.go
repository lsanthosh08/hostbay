package main

/*
#cgo CFLAGS: -x objective-c
#cgo LDFLAGS: -framework Cocoa
#import <Cocoa/Cocoa.h>

static void setDock(int visible) {
	[NSApp setActivationPolicy:visible ? NSApplicationActivationPolicyRegular : NSApplicationActivationPolicyAccessory];
	if (visible) {
		[NSApp activateIgnoringOtherApps:YES];
	}
}
*/
import "C"

import "github.com/wailsapp/wails/v3/pkg/application"

// showInDock puts Hostbay's icon in the Dock and Cmd-Tab, or takes it out
// so that only the menu bar icon remains while the window is hidden.
func showInDock(visible bool) {
	v := 0
	if visible {
		v = 1
	}
	application.InvokeSync(func() { C.setDock(C.int(v)) })
}
