//go:build !windows && cgo

package sshx

/*
#cgo linux LDFLAGS: -ldl
#include <dlfcn.h>
#include <stdint.h>
#include <stdlib.h>

// RFC 2744 types, declared here so no GSSAPI headers are needed to build:
// the library is loaded at run time. Apple's GSS.framework packs these
// structs to 2 bytes on Intel, as its gssapi.h does.
typedef uint32_t hb_uint32;
#if defined(__APPLE__) && (defined(__i386__) || defined(__x86_64__))
#pragma pack(push, 2)
#endif
typedef struct { size_t length; void *value; } hb_buffer;
typedef struct { hb_uint32 length; void *elements; } hb_oid;
#if defined(__APPLE__) && (defined(__i386__) || defined(__x86_64__))
#pragma pack(pop)
#endif

static hb_uint32 (*p_import_name)(hb_uint32 *, hb_buffer *, hb_oid *, void **);
static hb_uint32 (*p_init_sec_context)(hb_uint32 *, void *, void **, void *, hb_oid *, hb_uint32, hb_uint32,
	void *, hb_buffer *, hb_oid **, hb_buffer *, hb_uint32 *, hb_uint32 *);
static hb_uint32 (*p_get_mic)(hb_uint32 *, void *, hb_uint32, hb_buffer *, hb_buffer *);
static hb_uint32 (*p_release_buffer)(hb_uint32 *, hb_buffer *);
static hb_uint32 (*p_release_name)(hb_uint32 *, void **);
static hb_uint32 (*p_delete_sec_context)(hb_uint32 *, void **, hb_buffer *);
static hb_uint32 (*p_display_status)(hb_uint32 *, hb_uint32, int, hb_oid *, hb_uint32 *, hb_buffer *);

static int hb_load(const char *path) {
	void *l = dlopen(path, RTLD_NOW | RTLD_LOCAL);
	if (!l) return -1;
	p_import_name = dlsym(l, "gss_import_name");
	p_init_sec_context = dlsym(l, "gss_init_sec_context");
	p_get_mic = dlsym(l, "gss_get_mic");
	p_release_buffer = dlsym(l, "gss_release_buffer");
	p_release_name = dlsym(l, "gss_release_name");
	p_delete_sec_context = dlsym(l, "gss_delete_sec_context");
	p_display_status = dlsym(l, "gss_display_status");
	if (!p_import_name || !p_init_sec_context || !p_get_mic || !p_release_buffer || !p_release_name ||
		!p_delete_sec_context || !p_display_status) {
		dlclose(l);
		return -2;
	}
	return 0;
}

// GSS_C_NT_HOSTBASED_SERVICE and the Kerberos 5 mechanism. They are built
// on the stack: a packed static one would hold an unaligned pointer.
static hb_uint32 hb_import_name(hb_uint32 *minor, char *name, size_t len, void **out) {
	hb_buffer b = {len, name};
	hb_oid nt;
	nt.length = 10;
	nt.elements = "\x2a\x86\x48\x86\xf7\x12\x01\x02\x01\x04";
	return p_import_name(minor, &b, &nt, out);
}

// Mutual authentication and integrity; never GSS_C_DELEG_FLAG.
static hb_uint32 hb_init(hb_uint32 *minor, void **ctx, void *name, void *in, size_t inlen, hb_buffer *out, hb_uint32 *flags) {
	hb_buffer ib = {inlen, in};
	hb_oid krb5;
	krb5.length = 9;
	krb5.elements = "\x2a\x86\x48\x86\xf7\x12\x01\x02\x02";
	return p_init_sec_context(minor, NULL, ctx, name, &krb5, 2 | 32, 0, NULL, inlen ? &ib : NULL, NULL, out, flags, NULL);
}

static hb_uint32 hb_get_mic(hb_uint32 *minor, void *ctx, void *msg, size_t len, hb_buffer *out) {
	hb_buffer m = {len, msg};
	return p_get_mic(minor, ctx, 0, &m, out);
}

static void hb_release_buffer(hb_buffer *b) { hb_uint32 m; p_release_buffer(&m, b); }
static void hb_release_name(void **n) { hb_uint32 m; p_release_name(&m, n); }
static void hb_delete(void **ctx) { hb_uint32 m; p_delete_sec_context(&m, ctx, NULL); }

static hb_uint32 hb_display(hb_uint32 code, int type, hb_uint32 *more, hb_buffer *out) {
	hb_uint32 m;
	return p_display_status(&m, code, type, NULL, more, out);
}
*/
import "C"

import (
	"errors"
	"fmt"
	"runtime"
	"slices"
	"strings"
	"sync"
	"unsafe"

	"golang.org/x/crypto/ssh"
)

// The system GSSAPI library, so every credential cache kinit and SSSD use
// works, KCM: and KEYRING: included (R51).
var gssLibs = map[string][]string{
	"darwin": {"/System/Library/Frameworks/GSS.framework/GSS"},
	"linux":  {"libgssapi_krb5.so.2", "libgssapi.so.3"},
}

var (
	gssOnce sync.Once
	gssErr  error
)

func loadGSS() error {
	gssOnce.Do(func() {
		gssErr = errKerberosUnavailable
		for _, p := range gssLibs[runtime.GOOS] {
			cp := C.CString(p)
			ok := C.hb_load(cp) == 0
			C.free(unsafe.Pointer(cp))
			if ok {
				gssErr = nil
				return
			}
		}
	})
	return gssErr
}

const (
	gssContinueNeeded = 1
	gssMutualFlag     = 2
	gssRoutineMask    = 0xff << 16
	gssNoCred         = 7 << 16
	gssCredsExpired   = 11 << 16
)

// gssClient is one login's GSSAPI context (ssh.GSSAPIClient).
type gssClient struct {
	name unsafe.Pointer // gss_name_t
	ctx  unsafe.Pointer // gss_ctx_id_t
}

func newKerberosClient() (ssh.GSSAPIClient, error) {
	if err := loadGSS(); err != nil {
		return nil, err
	}
	return &gssClient{}, nil
}

func (g *gssClient) InitSecContext(target string, token []byte, _ bool) ([]byte, bool, error) {
	var minor C.hb_uint32
	if g.name == nil {
		ct := C.CString(target)
		defer C.free(unsafe.Pointer(ct))
		if major := C.hb_import_name(&minor, ct, C.size_t(len(target)), &g.name); major != 0 {
			return nil, false, gssError(major, minor)
		}
	}
	var in unsafe.Pointer
	if len(token) > 0 {
		in = C.CBytes(token)
		defer C.free(in)
	}
	var out C.hb_buffer
	var flags C.hb_uint32
	major := C.hb_init(&minor, &g.ctx, g.name, in, C.size_t(len(token)), &out, &flags)
	tok := C.GoBytes(out.value, C.int(out.length))
	C.hb_release_buffer(&out)
	if major&^gssContinueNeeded != 0 {
		return nil, false, gssError(major, minor)
	}
	more := major&gssContinueNeeded != 0
	if !more && flags&gssMutualFlag == 0 {
		return nil, false, errors.New("Kerberos: the server did not prove its identity (no mutual authentication)")
	}
	return tok, more, nil
}

func (g *gssClient) GetMIC(field []byte) ([]byte, error) {
	var minor C.hb_uint32
	msg := C.CBytes(field)
	defer C.free(msg)
	var out C.hb_buffer
	if major := C.hb_get_mic(&minor, g.ctx, msg, C.size_t(len(field)), &out); major != 0 {
		return nil, gssError(major, minor)
	}
	defer C.hb_release_buffer(&out)
	return C.GoBytes(out.value, C.int(out.length)), nil
}

func (g *gssClient) DeleteSecContext() error {
	if g.ctx != nil {
		C.hb_delete(&g.ctx)
		g.ctx = nil
	}
	if g.name != nil {
		C.hb_release_name(&g.name)
		g.name = nil
	}
	return nil
}

// gssError turns a GSSAPI status into an error; no ticket, or an expired
// one, is SecretNeeded with a hint to run kinit. Libraries report a
// missing cache in different ways, so the minor code and text count too.
func gssError(major, minor C.hb_uint32) error {
	msg := strings.TrimSpace(gssStatus(major, 1)) // GSS_C_GSS_CODE
	if m := gssStatus(minor, 2); m != "" {
		msg += ": " + m
	}
	routine := major & gssRoutineMask
	low := strings.ToLower(msg)
	if routine == gssNoCred || routine == gssCredsExpired || slices.Contains(noTicketCodes, int32(minor)) ||
		strings.Contains(low, "credentials cache") || strings.Contains(low, "no credentials") ||
		strings.Contains(low, "expired") || strings.Contains(low, "no such file") {
		return noTicket(msg)
	}
	return fmt.Errorf("Kerberos: %s", msg)
}

// noTicketCodes are krb5 minor codes for a missing cache or ticket:
// KRB5_FCC_NOFILE, KRB5_CC_NOTFOUND, KRB5KRB_AP_ERR_TKT_EXPIRED and ENOENT.
var noTicketCodes = []int32{-1765328189, -1765328243, -1765328352, 2}

func gssStatus(code C.hb_uint32, typ C.int) string {
	var parts []string
	var more C.hb_uint32
	for range 8 {
		var out C.hb_buffer
		if C.hb_display(code, typ, &more, &out) != 0 {
			break
		}
		parts = append(parts, C.GoStringN((*C.char)(out.value), C.int(out.length)))
		C.hb_release_buffer(&out)
		if more == 0 {
			break
		}
	}
	return strings.Join(parts, "; ")
}
