// A raw SSE reader of /api/events with a person's cookies, pinned to a replica. `events` collects the parsed
// `data:` objects; `replica` is the X-Replica header of the stream.
export async function openFeed(env, person, replica) {
  const cookies = await person.ctx.cookies(env.S.base);
  const ac = new AbortController();
  const r = await fetch(`${env.S.base}/api/events`, {
    headers: { cookie: cookies.map((c) => `${c.name}=${c.value}`).join("; "), accept: "text/event-stream", ...(replica === undefined ? {} : { "x-pin-replica": String(replica) }) },
    signal: ac.signal,
  });
  if (r.status !== 200) throw new Error(`/api/events -> ${r.status}`);
  const feed = { replica: r.headers.get("x-replica"), events: [], raw: "", close: () => ac.abort() };
  (async () => {
    const dec = new TextDecoder();
    try {
      for await (const chunk of r.body) {
        feed.raw += dec.decode(chunk, { stream: true });
        let i;
        while ((i = feed.raw.indexOf("\n\n")) >= 0) {
          const frame = feed.raw.slice(0, i);
          feed.raw = feed.raw.slice(i + 2);
          const ev = /^event: (.*)$/m.exec(frame)?.[1];
          const data = /^data: (.*)$/m.exec(frame)?.[1];
          if (data) {
            try {
              feed.events.push({ event: ev, ...JSON.parse(data) });
            } catch {
              feed.events.push({ event: ev, data });
            }
          }
        }
      }
    } catch {}
  })();
  return feed;
}
