# spice-html5 in Hostbay

These files are spice-html5 0.3.0, the SPICE client in JavaScript, from
https://gitlab.freedesktop.org/spice/spice-html5, tag spice-html5-0.3.0
(commit 56ddb3005e39a9e76670fec9676e667d44167bf3). The source archive,
https://gitlab.freedesktop.org/spice/spice-html5/-/archive/spice-html5-0.3.0/spice-html5-spice-html5-0.3.0.tar.gz,
had sha256 90fa3b30cda272471104af0b61ea08f2194048455133889a28a8bed0c00e3527.

spice-html5 is licensed under the GNU Lesser General Public License, version
3 or later (COPYING.LESSER, which supplements the GNU General Public License
version 3 in COPYING). This directory is its corresponding source as Hostbay
ships it: the build copies these files unminified into the app as their own
files under `vendor/spice-html5/`, loaded only when a SPICE tab opens, so
they can be replaced with another build of spice-html5 that keeps the same
module interface.

## What is included

- `src/*.js` from the release, as `*.js` here.
- `src/thirdparty/{jsbn,prng4,rng,rsa,sha1}.js` as `thirdparty/`. These keep
  their own notices in their headers: jsbn.js, prng4.js, rng.js and rsa.js
  are Copyright (c) 2003-2005 Tom Wu, under the permissive licence in each
  header; sha1.js is Copyright (c) 1998-2009 Paul Johnston and contributors,
  under the BSD licence in its header.
- Left out: `src/thirdparty/browser-es-module-loader/` (MIT), a Babel-based
  loader for browsers without ES modules that compiles in Web Workers. The
  client's modules do not import it and Hostbay's webviews load ES modules
  natively.

## Changes

One change, so that Hostbay can hand each channel a socket of its own (a
Wails GoStream) instead of a browser WebSocket. It does not touch
authentication: the client still links every channel itself.

`spiceconn.js`: a connection takes an optional `socket` option, a function
that returns a WebSocket-shaped object, or inherits its parent's. The main
channel builds the other channels from `{uri: this.ws.url, parent: this,
...}` (`main.js`), so setting it once on the main connection covers every
channel. Without the option the client behaves as before.

```diff
--- a/src/spiceconn.js
+++ b/src/spiceconn.js
@@ -50,7 +50,7 @@
     if (o === undefined || o.uri === undefined || ! o.uri)
         throw new Error("You must specify a uri");
 
-    this.ws = new WebSocket(o.uri, 'binary');
+    this.socket = o.socket || (o.parent && o.parent.socket); this.ws = this.socket ? this.socket(o) : new WebSocket(o.uri, 'binary');
 
     if (! this.ws.binaryType)
         throw new Error("WebSocket doesn't support binaryType.  Try a different browser.");
```
