/** What a sidebar conversation needs from Go. */
export type ConvBackend = {
  start: (provider: string, target: string) => Promise<string>;
  listen: (id: string) => Promise<() => void>;
  close: (id: string) => void;
};

const superseded = new Error("the conversation was ended while it started");

/** A start that end() overtook; the caller drops it quietly. */
export const isSuperseded = (e: unknown) => e === superseded;

/** The sidebar's one Go conversation. ensure returns the open one for a
 *  provider and target, or starts it; overlapping calls share one start,
 *  so a double send never opens a second conversation. A conversation
 *  ended while it was starting is closed as soon as Go returns it. */
export function conversations(b: ConvBackend) {
  let cur: { id: string; provider: string; target: string; unlisten: () => void } | null = null;
  let starting: { key: string; p: Promise<string> } | null = null;
  let gen = 0;

  const end = () => {
    gen++;
    starting = null;
    if (cur) {
      cur.unlisten();
      b.close(cur.id);
      cur = null;
    }
  };

  const ensure = (provider: string, target: string): Promise<string> => {
    if (cur && cur.provider === provider && cur.target === target) return Promise.resolve(cur.id);
    const key = `${provider}\0${target}`;
    if (starting?.key === key) return starting.p;
    end();
    const mine = gen;
    const p = (async () => {
      const id = await b.start(provider, target);
      let unlisten: () => void;
      try {
        unlisten = await b.listen(id);
      } catch (e) {
        b.close(id);
        throw e;
      }
      if (mine !== gen) {
        unlisten();
        b.close(id);
        throw superseded;
      }
      cur = { id, provider, target, unlisten };
      starting = null;
      return id;
    })();
    starting = { key, p };
    p.catch(() => {
      if (starting?.p === p) starting = null;
    });
    return p;
  };

  return { ensure, end, current: () => cur?.id ?? null };
}
