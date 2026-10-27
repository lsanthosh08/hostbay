// Bundle Monaco and its workers with the app. @monaco-editor/react loads it
// from a CDN by default, which the Content Security Policy (index.html) blocks.
import * as monaco from "monaco-editor";
import { loader } from "@monaco-editor/react";
import EditorWorker from "monaco-editor/editor/editor.worker?worker";
import JsonWorker from "monaco-editor/language/json/json.worker?worker";
import CssWorker from "monaco-editor/language/css/css.worker?worker";
import HtmlWorker from "monaco-editor/language/html/html.worker?worker";
import TsWorker from "monaco-editor/language/typescript/ts.worker?worker";
import { monacoTheme } from "./theme/terminal";
import { onThemeChange } from "./ui/theme";

self.MonacoEnvironment = {
  getWorker(_id, label) {
    if (label === "json") return new JsonWorker();
    if (label === "css" || label === "scss" || label === "less") return new CssWorker();
    if (label === "html" || label === "handlebars" || label === "razor") return new HtmlWorker();
    if (label === "typescript" || label === "javascript") return new TsWorker();
    return new EditorWorker();
  },
};

loader.config({ monaco });

// Editors take the app theme's colours and follow it when it changes. Called
// on an editor's mount, when the theme's CSS is surely applied.
let following = false;
export function applyEditorTheme() {
  const set = () => {
    monaco.editor.defineTheme("hostbay", monacoTheme());
    monaco.editor.setTheme("hostbay");
  };
  set();
  if (!following) {
    following = true;
    onThemeChange(set);
  }
}
