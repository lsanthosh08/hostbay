import React from "react";
import ReactDOM from "react-dom/client";
import App from "./App";
import { applyWindowPrefs } from "./components/WindowControls";
import { ToastProvider } from "./ui/toast";
import { ErrorBoundary } from "./ui/ErrorBoundary";
import { ThemeProvider } from "./ui/theme";
import { PromptProvider } from "./ui/prompt";
import "./styles.css";

// Disable the browser's default right-click menu app-wide (this is a desktop
// app, not a web page). Components add their own via useContextMenu().
window.addEventListener("contextmenu", (e) => e.preventDefault());

// Swallow file drops on the window. With Tauri dragDropEnabled=false the OS
// hands drops to the webview, whose default is to *open/navigate* to the
// dropped file (e.g. dropping an image onto the RDP canvas would replace the
// app with that image). Prevent that everywhere; drops are no-ops.
window.addEventListener("dragover", (e) => e.preventDefault());
window.addEventListener("drop", (e) => e.preventDefault());

// UI zoom and always-on-top from Settings > General.
applyWindowPrefs();

ReactDOM.createRoot(document.getElementById("root")!).render(
  <React.StrictMode>
    <ErrorBoundary>
      <ThemeProvider>
        <ToastProvider>
          <PromptProvider>
            <App />
          </PromptProvider>
        </ToastProvider>
      </ThemeProvider>
    </ErrorBoundary>
  </React.StrictMode>
);
