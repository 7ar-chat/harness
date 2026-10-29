import { useQuery } from "@tanstack/react-query";
import { Bell, CheckCircle2, ExternalLink, Send, Upload, User as UserIcon, Volume2 } from "lucide-react";
import { useCallback, useEffect, useRef, useState, type ReactNode } from "react";
import QRCode from "qrcode";
import {
  meQuery, toastError, useConnectors, useDisconnect, useRemoveAvatar, useSaveProfile, useTelegramLink, useTelegramTest, useUploadAvatar,
} from "@/api/queries";
import type { Connector } from "@/api/types";
import { Spinner, UserAvatar } from "@/components/app/common";
import { GithubMark, ImportGithubDialog } from "@/components/app/github-import";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { ScrollArea } from "@/components/ui/scroll-area";
import { Slider } from "@/components/ui/slider";
import { Switch } from "@/components/ui/switch";
import { useFeed } from "@/lib/feed";
import { blobToDataUrl } from "@/lib/utils";
import { playSound, setSoundPrefs, useSoundPrefs, type Sound } from "@/lib/sound";

// ---- avatar crop ----
const OUT = 256;
const VIEW = 256;

function CropDialog({ file, onClose, onDone }: { file: File | null; onClose: () => void; onDone: (b: Blob) => void }) {
  const canvas = useRef<HTMLCanvasElement>(null);
  const [img, setImg] = useState<HTMLImageElement | null>(null);
  const [zoom, setZoom] = useState(1);
  const [off, setOff] = useState({ x: 0, y: 0 });
  const drag = useRef<{ x: number; y: number } | null>(null);

  useEffect(() => {
    setImg(null);
    setZoom(1);
    setOff({ x: 0, y: 0 });
    if (!file) return;
    let alive = true;
    blobToDataUrl(file)
      .then((url) => {
        const i = new Image();
        i.onload = () => alive && setImg(i);
        i.onerror = () => toastError(new Error("That file is not a readable image."));
        i.src = url;
      })
      .catch(toastError);
    return () => {
      alive = false;
    };
  }, [file]);

  // Draws the square crop of `img` at size `s` (the source rectangle follows zoom and offset).
  const draw = useCallback(
    (c: HTMLCanvasElement, s: number) => {
      const g = c.getContext("2d");
      if (!g || !img) return;
      const side = Math.min(img.width, img.height) / zoom;
      const maxX = (img.width - side) / 2;
      const maxY = (img.height - side) / 2;
      const sx = img.width / 2 - side / 2 + Math.max(-maxX, Math.min(maxX, off.x * (side / VIEW)));
      const sy = img.height / 2 - side / 2 + Math.max(-maxY, Math.min(maxY, off.y * (side / VIEW)));
      c.width = c.height = s;
      g.fillStyle = "#fff";
      g.fillRect(0, 0, s, s);
      g.drawImage(img, sx, sy, side, side, 0, 0, s, s);
    },
    [img, zoom, off],
  );
  useEffect(() => {
    if (canvas.current) draw(canvas.current, VIEW);
  }, [draw]);

  const save = async () => {
    const out = document.createElement("canvas");
    draw(out, OUT);
    for (const q of [0.92, 0.8, 0.6, 0.4]) {
      const blob = await new Promise<Blob | null>((r) => out.toBlob(r, "image/jpeg", q));
      if (blob && blob.size < 400 * 1024) return onDone(blob);
    }
    toastError(new Error("The cropped image is still too large."));
  };

  return (
    <Dialog open={!!file} onOpenChange={(o) => !o && onClose()}>
      <DialogContent className="max-w-sm">
        <DialogHeader>
          <DialogTitle>Crop your avatar</DialogTitle>
          <DialogDescription>Drag to reposition, use the slider to zoom.</DialogDescription>
        </DialogHeader>
        <div className="flex justify-center">
          <canvas
            ref={canvas}
            width={VIEW}
            height={VIEW}
            className="size-64 cursor-grab touch-none rounded-full border bg-muted"
            aria-label="Avatar preview"
            onPointerDown={(e) => {
              e.currentTarget.setPointerCapture(e.pointerId);
              drag.current = { x: e.clientX, y: e.clientY };
            }}
            onPointerMove={(e) => {
              if (!drag.current) return;
              const dx = e.clientX - drag.current.x;
              const dy = e.clientY - drag.current.y;
              drag.current = { x: e.clientX, y: e.clientY };
              setOff((o) => ({ x: o.x - dx, y: o.y - dy }));
            }}
            onPointerUp={() => (drag.current = null)}
          />
        </div>
        <Slider min={1} max={3} step={0.01} value={[zoom]} onValueChange={([z]) => setZoom(z ?? 1)} aria-label="Zoom" />
        <DialogFooter>
          <Button variant="ghost" onClick={onClose}>
            Cancel
          </Button>
          <Button disabled={!img} onClick={() => void save()}>
            Use this photo
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

function ProfileCard() {
  const me = useQuery(meQuery).data?.user;
  const save = useSaveProfile();
  const upload = useUploadAvatar();
  const remove = useRemoveAvatar();
  const [name, setName] = useState("");
  const [file, setFile] = useState<File | null>(null);
  const input = useRef<HTMLInputElement>(null);
  useEffect(() => setName(me?.displayName ?? ""), [me?.displayName]);
  if (!me) return null;
  return (
    <Card>
      <CardHeader>
        <CardTitle className="flex items-center gap-2">
          <UserIcon className="size-4" /> Profile
        </CardTitle>
        <CardDescription>How teammates see you in sessions, messages and member lists.</CardDescription>
      </CardHeader>
      <CardContent className="space-y-6">
        <div className="flex items-center gap-4">
          <UserAvatar login={me.login} name={me.name} url={me.avatarUrl} className="size-20 text-xl" />
          <div className="flex flex-wrap gap-2">
            <input ref={input} type="file" accept="image/*" className="sr-only" aria-label="Choose an avatar image" onChange={(e) => (setFile(e.target.files?.[0] ?? null), (e.target.value = ""))} />
            <Button variant="outline" onClick={() => input.current?.click()} disabled={upload.isPending}>
              {upload.isPending ? <Spinner /> : <Upload />} Upload
            </Button>
            {me.hasCustomAvatar ? (
              <Button variant="ghost" onClick={() => remove.mutate()} disabled={remove.isPending}>
                Remove
              </Button>
            ) : null}
          </div>
        </div>
        <form
          className="max-w-md space-y-2"
          onSubmit={(e) => {
            e.preventDefault();
            save.mutate(name.trim());
          }}
        >
          <Label htmlFor="dn">Display name</Label>
          <div className="flex gap-2">
            <Input id="dn" value={name} onChange={(e) => setName(e.target.value)} placeholder={me.defaultName || me.login} maxLength={80} />
            <Button type="submit" disabled={save.isPending || name.trim() === (me.displayName ?? "")}>
              Save
            </Button>
            {me.displayName ? (
              <Button type="button" variant="ghost" onClick={() => save.mutate("")}>
                Reset to default
              </Button>
            ) : null}
          </div>
        </form>
        <dl className="grid max-w-md grid-cols-[6rem_1fr] gap-y-1 text-sm">
          <dt className="text-muted-foreground">Email</dt>
          <dd className="truncate">{me.email || "—"}</dd>
          <dt className="text-muted-foreground">Login</dt>
          <dd className="truncate">{me.login}</dd>
        </dl>
        <CropDialog file={file} onClose={() => setFile(null)} onDone={(b) => (setFile(null), upload.mutate(b))} />
      </CardContent>
    </Card>
  );
}

// ---- connectors ----
function statusOf(c: Connector): ReactNode {
  if (!c.configured) return <Badge variant="outline">Not configured</Badge>;
  if (!c.connected) return <Badge variant="secondary">Not connected</Badge>;
  return (
    <Badge className="gap-1">
      <CheckCircle2 className="size-3" /> Connected{c.account ? ` as ${c.account}` : ""}
    </Badge>
  );
}

function TelegramDialog({ open, onClose, link }: { open: boolean; onClose: () => void; link?: { url: string; expiresAt: string; code: string } }) {
  const [qr, setQr] = useState("");
  const [left, setLeft] = useState(0);
  // Poll until the bot reports the link as connected.
  const poll = useConnectors(open ? 2000 : undefined);
  const connected = poll.data?.find((c) => c.kind === "telegram")?.connected;
  useEffect(() => {
    if (link?.url) void QRCode.toDataURL(link.url, { margin: 1, width: 220 }).then(setQr).catch(() => setQr(""));
  }, [link?.url]);
  useEffect(() => {
    if (!link) return;
    const tick = () => setLeft(Math.max(0, Math.round((new Date(link.expiresAt).getTime() - Date.now()) / 1000)));
    tick();
    const t = setInterval(tick, 1000);
    return () => clearInterval(t);
  }, [link]);
  useEffect(() => {
    if (open && connected) onClose();
  }, [open, connected, onClose]);
  return (
    <Dialog open={open} onOpenChange={(o) => !o && onClose()}>
      <DialogContent className="max-w-sm">
        <DialogHeader>
          <DialogTitle>Connect Telegram</DialogTitle>
          <DialogDescription>Open the link on your phone (or scan the code) and press Start in the chat.</DialogDescription>
        </DialogHeader>
        <div className="flex flex-col items-center gap-3">
          {qr ? <img src={qr} alt="QR code for the Telegram link" className="size-[220px] rounded-md border bg-white" /> : <Spinner />}
          {link ? (
            <Button asChild>
              <a href={link.url} target="_blank" rel="noreferrer">
                <Send /> Open in Telegram
              </a>
            </Button>
          ) : null}
          <p className="text-xs text-muted-foreground" aria-live="polite">
            {left > 0 ? `Waiting for you… the link expires in ${Math.floor(left / 60)}:${String(left % 60).padStart(2, "0")}` : "This link has expired. Close and try again."}
          </p>
        </div>
      </DialogContent>
    </Dialog>
  );
}

function ConnectorCard({ c, onImport }: { c: Connector; onImport: () => void }) {
  const disconnect = useDisconnect(c.kind);
  const link = useTelegramLink();
  const test = useTelegramTest();
  const [dlg, setDlg] = useState(false);
  const closeDlg = useCallback(() => setDlg(false), []);
  const gh = c.kind === "github";
  return (
    <Card>
      <CardHeader>
        <CardTitle className="flex flex-wrap items-center gap-2">
          {gh ? <GithubMark className="size-4" /> : <Send className="size-4" />}
          {c.label || (gh ? "GitHub" : "Telegram")}
          {statusOf(c)}
        </CardTitle>
        <CardDescription>{gh ? "Import repositories as projects and let agents work on them." : "Chat with your assistant from Telegram; it also pings you when a session needs you."}</CardDescription>
      </CardHeader>
      <CardContent className="space-y-3">
        {!c.configured && c.message ? <p className="text-sm text-muted-foreground">{c.message}</p> : null}
        {c.configured && !c.connected && c.message ? <p className="text-sm text-muted-foreground">{c.message}</p> : null}
        <div className="flex flex-wrap gap-2">
          {c.configured && !c.connected && gh ? (
            <Button onClick={() => c.connectUrl && location.assign(c.connectUrl)} disabled={!c.connectUrl}>
              <GithubMark className="size-4" /> Connect GitHub
            </Button>
          ) : null}
          {c.configured && !c.connected && !gh ? (
            <Button
              disabled={link.isPending}
              onClick={() => link.mutate(undefined, { onSuccess: () => setDlg(true) })}
            >
              {link.isPending ? <Spinner /> : <Send />} Connect Telegram
            </Button>
          ) : null}
          {c.connected && gh ? (
            <Button onClick={onImport}>
              <ExternalLink /> Import a project from GitHub
            </Button>
          ) : null}
          {c.connected && !gh ? (
            <Button variant="outline" onClick={() => test.mutate()} disabled={test.isPending}>
              Send test message
            </Button>
          ) : null}
          {c.connected ? (
            <Button variant="ghost" onClick={() => disconnect.mutate()} disabled={disconnect.isPending}>
              Disconnect
            </Button>
          ) : null}
        </div>
        {!gh ? <TelegramDialog open={dlg} onClose={closeDlg} link={link.data} /> : null}
      </CardContent>
    </Card>
  );
}

function Connectors() {
  const { data, isLoading } = useConnectors();
  const [imp, setImp] = useState(false);
  return (
    <section className="space-y-3" aria-labelledby="conn-h">
      <h2 id="conn-h" className="text-sm font-medium text-muted-foreground">
        Connectors
      </h2>
      {isLoading ? <Spinner /> : null}
      {data?.length === 0 ? <p className="text-sm text-muted-foreground">No connectors are available on this server.</p> : null}
      {data?.map((c) => <ConnectorCard key={c.kind} c={c} onImport={() => setImp(true)} />)}
      <ImportGithubDialog open={imp} onClose={() => setImp(false)} />
    </section>
  );
}

// ---- sounds and notifications ----
const SOUNDS: [Sound, string][] = [
  ["success", "Finished"],
  ["help", "Needs you"],
  ["failed", "Failed"],
];

function Preferences() {
  const p = useSoundPrefs();
  const { notifyPermission, askNotify } = useFeed();
  return (
    <Card>
      <CardHeader>
        <CardTitle className="flex items-center gap-2">
          <Volume2 className="size-4" /> Sounds and notifications
        </CardTitle>
        <CardDescription>Stored in this browser only.</CardDescription>
      </CardHeader>
      <CardContent className="space-y-5">
        <div className="flex items-center gap-3">
          <Switch id="snd" checked={p.enabled} onCheckedChange={(enabled) => setSoundPrefs({ enabled })} />
          <Label htmlFor="snd">Play sounds when a session finishes or needs you</Label>
        </div>
        <div className="flex max-w-sm items-center gap-3">
          <Label className="w-16 text-sm">Volume</Label>
          <Slider min={0} max={1} step={0.05} value={[p.volume]} disabled={!p.enabled} onValueChange={([volume]) => setSoundPrefs({ volume: volume ?? 0.5 })} aria-label="Volume" />
        </div>
        <div className="flex flex-wrap gap-2">
          {SOUNDS.map(([s, label]) => (
            <Button key={s} variant="outline" size="sm" onClick={() => playSound(s, p.volume)}>
              Preview: {label}
            </Button>
          ))}
        </div>
        <div className="flex items-center gap-3 border-t pt-4">
          <Bell className="size-4" />
          <div className="flex-1 text-sm">
            Browser notifications
            <div className="text-xs text-muted-foreground">
              {notifyPermission === "granted" ? "Enabled." : notifyPermission === "denied" ? "Blocked in your browser settings." : notifyPermission === "unsupported" ? "Not supported by this browser." : "Get a desktop notification when a session needs you and the tab is in the background."}
            </div>
          </div>
          {notifyPermission === "default" ? (
            <Button size="sm" onClick={askNotify}>
              Enable
            </Button>
          ) : null}
        </div>
      </CardContent>
    </Card>
  );
}

export function ProfilePage() {
  return (
    <ScrollArea className="h-full">
      <div className="mx-auto max-w-2xl space-y-6 p-4 md:p-8">
        <h1 className="text-xl font-semibold">Profile</h1>
        <ProfileCard />
        <Connectors />
        <Preferences />
      </div>
    </ScrollArea>
  );
}
