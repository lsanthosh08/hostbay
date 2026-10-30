import { errorInfo, type AppError } from "../api";

/** A gateway certificate a failed connect asks about (R21): the pin
 *  presented and, for a changed one, the saved pin it does not match (AE8),
 *  which is refused and can only be forgotten. */
export type VpnCert = { pin: string; pinned?: string };

/** The certificate a VPNFailed error asks about, if it does; `vpnId` is the
 *  profile whose gateway presented it (host connects name it too). */
export function vpnCertOf({ code, details }: AppError): VpnCert | undefined {
  const reason = details?.reason;
  if (code !== "VPNFailed" || (reason !== "cert-unknown" && reason !== "cert-changed") || !details?.serverCert) return undefined;
  return reason === "cert-changed" ? { pin: details.serverCert, pinned: details.pinned ?? "" } : { pin: details.serverCert };
}

/** The certificate a rejected connectVpn asks about, if it does. */
export const vpnCertPrompt = (e: unknown) => vpnCertOf(errorInfo(e));
