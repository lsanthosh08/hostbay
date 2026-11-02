// hostbay-sso signs in to an AnyConnect gateway whose SAML login only runs in
// an embedded browser (the gateway never offers external-browser mode, so the
// openconnect command line fails with "No SSO handler"). It does what a GUI
// openconnect front end does: libopenconnect drives the gateway, and this
// program supplies the web view. The session cookie it prints is then handed
// to openconnect with --cookie-on-stdin.
//
//	hostbay-sso --gateway HOST[:PORT] [--group NAME] [--user NAME] [--pin pin-sha256:...]
//	hostbay-sso --gateway HOST[:PORT] --list-groups
//
// --list-groups prints {"groups":["..."],"default":"..."}, the sign-in groups
// the gateway offers, and signs in to nothing.
//
// One JSON line goes to stdout, and the exit status is 0 only for a cookie:
//
//	{"cookie":"...","pin":"pin-sha256:..."}
//	{"error":"...","code":"cert-unknown|cert-changed|cancelled|failed","pin":"..."}
//
// The cookie is never logged and never an argument. The sign-in window keeps
// nothing: its web data lives only as long as this process.
#import <Cocoa/Cocoa.h>
#import <WebKit/WebKit.h>
#include <openconnect.h>
#include <pthread.h>
#include <dispatch/dispatch.h>
#include <errno.h>
#include <string.h>
#include <strings.h>

static struct openconnect_info *vpn;
static dispatch_semaphore_t windowDone;
static int windowResult = -1;
static BOOL windowFinished;
static NSWindow *window;
static WKWebView *webView;

static NSString *gateway, *group, *loginUser, *wantedPin;
static BOOL listGroups;
static NSMutableArray *groupNames;
static NSString *defaultGroup;
static char seenPin[128]; // the pin of a gateway certificate the system does not trust

static void emit(NSDictionary *d) {
	NSData *j = [NSJSONSerialization dataWithJSONObject:d options:0 error:nil];
	fwrite(j.bytes, 1, j.length, stdout);
	fputc('\n', stdout);
	fflush(stdout);
}

static void fail(NSString *code, NSString *msg, NSString *pin) {
	emit(@{@"error": msg, @"code": code, @"pin": pin ?: @""});
	exit(1);
}

static char lastError[512]; // openconnect's last error line, for the reason shown to the user

static void progress(void *p, int level, const char *fmt, ...) {
	if (level > PRG_ERR) return; // errors only: nothing here may carry a secret
	va_list ap;
	va_start(ap, fmt);
	vsnprintf(lastError, sizeof lastError, fmt, ap);
	va_end(ap);
	size_t n = strlen(lastError);
	while (n && (lastError[n - 1] == '\n' || lastError[n - 1] == '\r')) lastError[--n] = 0;
	fprintf(stderr, "%s\n", lastError);
}

// validate runs only for a certificate the system does not trust. The one the
// profile pinned is accepted; anything else is refused and reported by pin.
static int validate(void *p, const char *reason) {
	const char *h = openconnect_get_peer_cert_hash(vpn);
	if (wantedPin.length && h && !strcmp(h, wantedPin.UTF8String)) return 0;
	snprintf(seenPin, sizeof seenPin, "%s", h ?: "");
	return 1;
}

// form picks the gateway group, matched without regard to case. The
// gateway's own default stays when no group is set or none matches.
static int form(void *p, struct oc_auth_form *f) {
	if (listGroups) { // only the gateway's group list is wanted: stop before any sign-in
		if (f->authgroup_opt) {
			struct oc_form_opt_select *sel = f->authgroup_opt;
			for (int i = 0; i < sel->nr_choices; i++) [groupNames addObject:@(sel->choices[i]->name)];
			if (f->authgroup_selection >= 0 && f->authgroup_selection < sel->nr_choices) defaultGroup = @(sel->choices[f->authgroup_selection]->name);
		}
		return OC_FORM_RESULT_CANCELLED;
	}
	if (!group.length || !f->authgroup_opt) return OC_FORM_RESULT_OK;
	struct oc_form_opt_select *sel = f->authgroup_opt;
	for (int i = 0; i < sel->nr_choices; i++) {
		struct oc_choice *c = sel->choices[i];
		if (strcasecmp(c->name, group.UTF8String) && (!c->label || strcasecmp(c->label, group.UTF8String))) continue;
		if (i == f->authgroup_selection) return OC_FORM_RESULT_OK;
		f->authgroup_selection = i;
		openconnect_set_option_value(&sel->form, c->name);
		return OC_FORM_RESULT_NEWGROUP;
	}
	return OC_FORM_RESULT_OK;
}

static void finishWindow(int result) {
	if (windowFinished) return;
	windowFinished = YES;
	windowResult = result;
	[window close];
	dispatch_semaphore_signal(windowDone);
}

@interface Delegate : NSObject <WKNavigationDelegate, NSWindowDelegate>
@end

@implementation Delegate

// The user name is offered to Microsoft's sign-in page, which then skips its
// e-mail prompt. login_hint is a request, never a credential.
- (void)webView:(WKWebView *)wv decidePolicyForNavigationAction:(WKNavigationAction *)action decisionHandler:(void (^)(WKNavigationActionPolicy))handler {
	NSURL *u = action.request.URL;
	if (loginUser.length && action.targetFrame.isMainFrame && [u.host isEqualToString:@"login.microsoftonline.com"] &&
	    ![u.query containsString:@"login_hint="]) {
		// Appended to the URL as it is: the SAML request in it is signed and
		// deflated, and decoding then encoding its query would corrupt it.
		NSString *hint = [loginUser stringByAddingPercentEncodingWithAllowedCharacters:[NSCharacterSet alphanumericCharacterSet]];
		NSURL *hinted = [NSURL URLWithString:[u.absoluteString stringByAppendingFormat:@"&login_hint=%@", hint]];
		if (hinted) {
			handler(WKNavigationActionPolicyCancel);
			dispatch_async(dispatch_get_main_queue(), ^{ [wv loadRequest:[NSURLRequest requestWithURL:hinted]]; });
			return;
		}
	}
	handler(WKNavigationActionPolicyAllow);
}

- (void)webView:(WKWebView *)wv didFinishNavigation:(WKNavigation *)nav {
	NSString *uri = wv.URL.absoluteString;
	if (getenv("HOSTBAY_SSO_TRACE")) fprintf(stderr, "loaded %s%s\n", wv.URL.host.UTF8String, wv.URL.path.UTF8String);
	[wv.configuration.websiteDataStore.httpCookieStore getAllCookies:^(NSArray<NSHTTPCookie *> *cs) {
		if (windowFinished) return;
		const char **c = calloc(cs.count * 2 + 1, sizeof(char *));
		int i = 0;
		for (NSHTTPCookie *k in cs) {
			c[i++] = k.name.UTF8String;
			c[i++] = k.value.UTF8String;
		}
		struct oc_webview_result r = {.uri = uri.UTF8String, .cookies = c, .headers = NULL};
		int rc = openconnect_webview_load_changed(vpn, &r);
		free(c);
		if (rc != -EAGAIN) finishWindow(rc); // 0: signed in
	}];
}

// A page that fails to load would otherwise leave the window blank.
static void loadFailed(WKWebView *wv, NSError *e) {
	// Our own cancelled navigation (the login_hint reload) and a redirect handing over are not failures.
	if (e.code == NSURLErrorCancelled || ([e.domain isEqualToString:@"WebKitErrorDomain"] && e.code == 102)) return;
	NSString *msg = [NSString stringWithFormat:@"The sign-in page did not load: %@", e.localizedDescription];
	fprintf(stderr, "%s\n", msg.UTF8String);
	NSString *html = [NSString stringWithFormat:@"<body style='font:15px -apple-system;padding:2em'><h3>Sign-in page did not load</h3><p>%@</p><p>Close this window and try again.</p>", msg];
	[wv loadHTMLString:html baseURL:nil];
}
- (void)webView:(WKWebView *)wv didFailProvisionalNavigation:(WKNavigation *)nav withError:(NSError *)e { loadFailed(wv, e); }
- (void)webView:(WKWebView *)wv didFailNavigation:(WKNavigation *)nav withError:(NSError *)e { loadFailed(wv, e); }
- (void)webViewWebContentProcessDidTerminate:(WKWebView *)wv { fprintf(stderr, "web content process ended\n"); [wv reload]; }

- (void)windowWillClose:(NSNotification *)n {
	finishWindow(-ECANCELED);
}
@end

static Delegate *delegate;

static int openWebview(struct openconnect_info *v, const char *uri, void *p) {
	if (listGroups) return -ECANCELED; // the library asks for the sign-in even after the form is cancelled
	NSString *u = @(uri);
	dispatch_async(dispatch_get_main_queue(), ^{
		WKWebViewConfiguration *cfg = [WKWebViewConfiguration new];
		cfg.websiteDataStore = [WKWebsiteDataStore nonPersistentDataStore];
		webView = [[WKWebView alloc] initWithFrame:NSMakeRect(0, 0, 520, 720) configuration:cfg];
		delegate = [Delegate new];
		webView.navigationDelegate = delegate;
		window = [[NSWindow alloc] initWithContentRect:NSMakeRect(0, 0, 520, 720)
		                                     styleMask:NSWindowStyleMaskTitled | NSWindowStyleMaskClosable | NSWindowStyleMaskResizable
		                                       backing:NSBackingStoreBuffered
		                                         defer:NO];
		window.delegate = delegate;
		window.title = @"Sign in to VPN";
		window.contentView = webView;
		window.releasedWhenClosed = NO;
		[window center];
		[window makeKeyAndOrderFront:nil];
		[NSApp activateIgnoringOtherApps:YES];
		[webView loadRequest:[NSURLRequest requestWithURL:[NSURL URLWithString:u]]];
	});
	dispatch_semaphore_wait(windowDone, DISPATCH_TIME_FOREVER);
	return windowResult;
}

// openBrowser serves a gateway that does offer external-browser mode: the
// library listens for the answer itself, so only the page needs opening.
static int openBrowser(struct openconnect_info *v, const char *uri, void *p) {
	if (listGroups) return -ECANCELED;
	NSURL *u = [NSURL URLWithString:@(uri)];
	if (!u) return -EINVAL;
	dispatch_async(dispatch_get_main_queue(), ^{ [[NSWorkspace sharedWorkspace] openURL:u]; });
	return 0;
}

static void *worker(void *arg) {
	vpn = openconnect_vpninfo_new("AnyConnect-compatible OpenConnect VPN Agent", validate, NULL, form, progress, NULL);
	if (!vpn) fail(@"failed", @"Cannot start OpenConnect.", nil);
	openconnect_set_protocol(vpn, "anyconnect");
	if (openconnect_parse_url(vpn, gateway.UTF8String)) fail(@"failed", @"The gateway is not a host name or address.", nil);
	openconnect_set_webview_callback(vpn, openWebview);
	openconnect_set_external_browser_callback(vpn, openBrowser);
	int rc = openconnect_obtain_cookie(vpn);
	if (listGroups) {
		if (seenPin[0]) fail(@"cert-unknown", @"The gateway's certificate is not trusted.", @(seenPin));
		emit(@{@"groups": groupNames, @"default": defaultGroup ?: @""});
		exit(0);
	}
	const char *h = openconnect_get_peer_cert_hash(vpn);
	if (rc == 0) {
		const char *ck = openconnect_get_cookie(vpn);
		if (wantedPin.length && h && strcmp(h, wantedPin.UTF8String)) fail(@"cert-changed", @"The gateway is not the pinned one.", @(h));
		if (!ck || !*ck) fail(@"failed", @"The gateway gave no session cookie.", nil);
		emit(@{@"cookie": @(ck), @"pin": @(h ?: "")});
		exit(0);
	}
	if (seenPin[0]) fail(wantedPin.length ? @"cert-changed" : @"cert-unknown", @"The gateway's certificate is not trusted.", @(seenPin));
	if (windowFinished && windowResult == -ECANCELED) fail(@"cancelled", @"Sign-in was cancelled.", nil);
	fail(@"failed", lastError[0] ? @(lastError) : @"Failed to complete authentication", nil);
	return NULL;
}

int main(int argc, char **argv) {
	@autoreleasepool {
		for (int i = 1; i < argc; i++) {
			NSString *k = @(argv[i]);
			if ([k isEqualToString:@"--version"]) { // the bundling script checks the binary runs
				puts("hostbay-sso 1");
				return 0;
			}
			if ([k isEqualToString:@"--list-groups"]) {
				listGroups = YES;
				continue;
			}
			if (i + 1 >= argc) break;
			NSString *v = @(argv[++i]);
			if ([k isEqualToString:@"--gateway"]) gateway = v;
			else if ([k isEqualToString:@"--group"]) group = v;
			else if ([k isEqualToString:@"--user"]) loginUser = v;
			else if ([k isEqualToString:@"--pin"]) wantedPin = v;
		}
		groupNames = [NSMutableArray array];
		if (!gateway.length) fail(@"failed", @"No gateway.", nil);
		[NSApplication sharedApplication];
		[NSApp setActivationPolicy:listGroups ? NSApplicationActivationPolicyProhibited : NSApplicationActivationPolicyRegular];
		windowDone = dispatch_semaphore_create(0);
		openconnect_init_ssl();
		pthread_t t;
		pthread_create(&t, NULL, worker, NULL);
		[NSApp run];
	}
	return 0;
}
