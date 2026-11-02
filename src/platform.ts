import { System } from "@wailsio/runtime";

/** Whether this is macOS; false where the runtime is not there (tests, a browser). */
export const isMac = () => {
  try {
    return System.IsMac();
  } catch {
    return false;
  }
};
