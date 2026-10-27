import { HostManagerProvider } from "./components/HostManager";
import { MasterPasswordPrompts } from "./components/MasterPassword";
import { AppShell } from "./shell/AppShell";
import { HostsProvider } from "./state/hosts";
import { SessionsProvider } from "./state/SessionsProvider";
import { TransfersProvider } from "./state/TransfersProvider";

export default function App() {
  return (
    <HostsProvider>
      <HostManagerProvider>
        <SessionsProvider>
          <TransfersProvider>
            <AppShell />
          </TransfersProvider>
        </SessionsProvider>
      </HostManagerProvider>
      <MasterPasswordPrompts />
    </HostsProvider>
  );
}
