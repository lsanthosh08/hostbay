// The part of zmodem.js 0.1.10 (CommonJS, untyped) that src/tabs/zmodem.ts uses.
declare module "zmodem.js" {
  type Octets = number[];

  namespace Zmodem {
    interface Detection {
      /** "receive" for a server's sz (ZRQINIT), "send" for its rz (ZRINIT). */
      get_session_role(): "receive" | "send";
      confirm(): Session;
    }

    interface Offer {
      get_details(): { name: string; size?: number };
      accept(opts: { on_input: (payload: Octets) => void }): Promise<unknown>;
      skip(): void;
    }

    interface Transfer {
      send(bytes: Uint8Array | Octets): Promise<void>;
      /** Resolves once the receiver has the whole file. */
      end(bytes?: Uint8Array | Octets): Promise<void>;
    }

    interface Session {
      type: "receive" | "send";
      /** Either end cancelled it. */
      aborted(): boolean;
      on(event: "offer", cb: (offer: Offer) => void): this;
      on(event: "session_end", cb: () => void): this;
      /** Receive only: asks the sender for its first file. */
      start(): Promise<unknown>;
      /** Send only: resolves to undefined when the receiver skips the file. */
      send_offer(params: { name: string; size: number }): Promise<Transfer | undefined>;
      /** Send only: ends the session once the receiver agrees. */
      close(): Promise<void>;
    }

    class Sentry {
      constructor(opts: {
        to_terminal: (octets: Octets) => void;
        sender: (octets: Octets) => void;
        on_detect: (d: Detection) => void;
        on_retract: () => void;
      });
      consume(input: ArrayBuffer | Uint8Array | Octets): void;
    }
  }

  const Zmodem: { Sentry: typeof Zmodem.Sentry };
  export default Zmodem;
}
