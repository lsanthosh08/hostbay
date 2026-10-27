// The part of noVNC 1.7.0 (@novnc/novnc, untyped ES modules) that
// src/tabs/VncTab.tsx uses; docs/API.md in the package describes it all.
declare module "@novnc/novnc" {
  export default class RFB extends EventTarget {
    /** channel: an open WebSocket-shaped object (here the "vnc" GoStream). */
    constructor(target: HTMLElement, channel: WebSocket, options?: { shared?: boolean });
    viewOnly: boolean;
    scaleViewport: boolean;
    resizeSession: boolean;
    clipViewport: boolean;
    focusOnClick: boolean;
    background: string;
    disconnect(): void;
    sendCtrlAltDel(): void;
    clipboardPasteFrom(text: string): void;
    focus(options?: FocusOptions): void;
    blur(): void;
  }
}
