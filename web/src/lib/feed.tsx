// The live status feed (GET /api/events): keeps the overview and open threads fresh,
// raises toasts and browser notifications when a session needs a person.
import { useQueryClient } from "@tanstack/react-query";
import { useLocation, useNavigate } from "@tanstack/react-router";
import { createContext, useContext, useEffect, useRef, useState, type ReactNode } from "react";
import { toast } from "sonner";
import { openSse } from "@/lib/sse";
import { cue, soundFor } from "@/lib/sound";
import { qk, useMe } from "@/api/queries";
import type { FeedEvent, Overview, ThreadStatus } from "@/api/types";

type Conn = "connecting" | "live" | "offline";
const FeedCtx = createContext<{ conn: Conn; notifyPermission: NotificationPermission | "unsupported"; askNotify: () => void }>({
  conn: "connecting",
  notifyPermission: "unsupported",
  askNotify: () => {},
});
export const useFeed = () => useContext(FeedCtx);

const finishedToStatus: Record<string, ThreadStatus> = {
  completed: "idle",
  cancelled: "idle",
  failed: "failed",
  interrupted: "failed",
  waiting_approval: "waiting_approval",
  waiting_input: "waiting_input",
};

const NEEDS_YOU: Record<string, string> = {
  waiting_approval: "is waiting for your approval",
  waiting_input: "has a question for you",
  failed: "failed",
};

/** Mirror a feed event into the cached overview so the sidebar updates before the refetch lands. */
export function patchOverview(o: Overview, ev: FeedEvent): Overview {
  const patch = (list: Overview["personal"]) =>
    list.flatMap((t) => {
      if (t.id !== ev.threadId) return [t];
      if (ev.type === "thread.deleted") return [];
      let status = t.status;
      if (ev.type === "run.started") status = "running";
      else if (ev.type === "run.finished" && ev.status) status = finishedToStatus[ev.status] ?? status;
      return [{ ...t, status, title: ev.title || t.title, updatedAt: ev.at }];
    });
  return { personal: patch(o.personal), projects: o.projects.map((p) => ({ ...p, threads: patch(p.threads) })) };
}

export function FeedProvider({ children }: { children: ReactNode }) {
  const qc = useQueryClient();
  const navigate = useNavigate();
  const me = useMe().data?.user.login;
  const meRef = useRef(me);
  meRef.current = me;
  const pathname = useLocation({ select: (l) => l.pathname });
  const openThread = useRef<string>();
  openThread.current = /^\/t\/([^/]+)/.exec(pathname)?.[1];
  const [conn, setConn] = useState<Conn>("connecting");
  const [perm, setPerm] = useState<NotificationPermission | "unsupported">(typeof Notification === "undefined" ? "unsupported" : Notification.permission);
  const timer = useRef<number>();

  useEffect(() => {
    const refresh = () => {
      window.clearTimeout(timer.current);
      timer.current = window.setTimeout(() => void qc.invalidateQueries({ queryKey: qk.overview }), 250);
    };
    const handle = (ev: FeedEvent) => {
      const mine = ev.actor !== undefined && ev.actor === meRef.current;
      if (ev.threadId && (ev.type === "run.started" || ev.type === "run.finished" || ev.type === "message.sent" || ev.type === "thread.updated")) {
        qc.setQueryData<Overview>(qk.overview, (o) => (o ? patchOverview(o, ev) : o));
        if (!mine || ev.type === "run.finished") void qc.invalidateQueries({ queryKey: qk.thread(ev.threadId) });
      }
      if (ev.type === "thread.deleted" && ev.threadId) qc.removeQueries({ queryKey: qk.thread(ev.threadId) });
      if (ev.type === "member.changed" || ev.type === "project.changed") {
        void qc.invalidateQueries({ queryKey: qk.me });
        if (ev.projectId) void qc.invalidateQueries({ queryKey: ["project", ev.projectId] });
      }
      if (ev.type === "memory.changed" || ev.type === "run.finished") void qc.invalidateQueries({ queryKey: ["memory"] });
      const sound = soundFor(ev, { me: meRef.current, watching: document.hasFocus() && !document.hidden ? openThread.current : undefined });
      if (sound) cue(sound);
      if (ev.type === "run.finished" && ev.status && NEEDS_YOU[ev.status] && ev.threadId) {
        const id = ev.threadId;
        const title = ev.title || "A session";
        const text = `${title} ${NEEDS_YOU[ev.status]}`;
        toast(text, { action: { label: "Open", onClick: () => void navigate({ to: "/t/$threadId", params: { threadId: id } }) } });
        if (typeof Notification !== "undefined" && Notification.permission === "granted" && document.hidden) {
          const n = new Notification("Harness", { body: text, tag: id });
          n.onclick = () => {
            window.focus();
            void navigate({ to: "/t/$threadId", params: { threadId: id } });
          };
        }
      }
      refresh();
    };
    const close = openSse({
      url: "/api/events",
      onOpen: (reconnect) => {
        setConn("live");
        if (reconnect) void qc.invalidateQueries();
      },
      onError: () => setConn("offline"),
      onMessage: (m) => {
        if (!m.data) return;
        try {
          handle(JSON.parse(m.data) as FeedEvent);
        } catch {
          /* ignore malformed event */
        }
      },
    });
    return () => {
      close();
      window.clearTimeout(timer.current);
    };
  }, [qc, navigate]);

  const askNotify = () => {
    if (typeof Notification === "undefined") return;
    void Notification.requestPermission().then(setPerm);
  };
  return <FeedCtx.Provider value={{ conn, notifyPermission: perm, askNotify }}>{children}</FeedCtx.Provider>;
}
