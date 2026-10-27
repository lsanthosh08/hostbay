import { createElement, useCallback, useState } from "react";
import * as api from "../api";
import type { Host, HostKeyChange, HostKeyPrompt } from "../api";
import { TrustDialog } from "../components/TrustDialog";
import { useToast } from "../ui/toast";
import { route, trustService, type Step } from "./router";

/** Answers a trust step with the service that asked (router.trustService). */
export const answerTrust = (prompt: HostKeyPrompt, accept: boolean) =>
  ({
    rdp: api.rdpTrustCert,
    vnc: api.vncTrustCert,
    spice: api.spiceTrustCert,
    database: api.databaseTrustCert,
    kubernetes: api.kubernetesTrustCert,
    transfer: api.transferTrustCert,
    ssh: api.trustHostKey,
  })[trustService(prompt)](prompt.requestId, accept);

/** Removes the key or pinned certificate a keyChanged step refused, with the
 *  service that refused it: a database server's pin, else an SSH host key. */
export const removeKey = (change: HostKeyChange, typedHost: string) =>
  ({ database: api.databaseRemovePin, kubernetes: api.kubernetesRemovePin }[trustService({ kind: change.kind } as HostKeyPrompt) as string] ?? api.removeHostKey)(
    change.removeId,
    typedHost
  );

/**
 * The connect flow for the calls outside a session tab (Tunnels, Sync, RDP).
 * `fail` routes a rejected call: a trust step shows the trust dialog and runs
 * `retry` once trusted; every other step goes to `onStep`, which defaults to
 * an error toast. Render `dialog` once.
 */
export function useConnect() {
  const { push } = useToast();
  const [trust, setTrust] = useState<{ prompt: HostKeyPrompt; retry: () => void; onStep: (s: Step) => void } | null>(null);
  const toast = useCallback((s: Step) => push("error", s.message), [push]);

  const fail = useCallback(
    (e: unknown, retry: () => void, opts: { onStep?: (s: Step) => void; host?: Host } = {}) => {
      const onStep = opts.onStep ?? toast;
      const step = route(api.errorInfo(e), { host: opts.host });
      if (step.kind === "trust") setTrust({ prompt: step.prompt, retry, onStep });
      else onStep(step);
    },
    [toast]
  );

  const dialog =
    trust &&
    createElement(TrustDialog, {
      prompt: trust.prompt,
      onConfirm: () => {
        const { prompt, retry, onStep } = trust;
        setTrust(null);
        answerTrust(prompt, true).then(retry, (e) => onStep({ kind: "failure", message: api.errorInfo(e).message }));
      },
      onCancel: () => {
        answerTrust(trust.prompt, false).catch(() => {});
        setTrust(null);
      },
    });

  return { fail, dialog };
}
