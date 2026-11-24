// The keyboard shortcuts sheet, opened with "?" or from the palette and the help center.
import { useEscapeClose } from "@/components/app/help";
import { Sheet, SheetContent, SheetDescription, SheetHeader, SheetTitle } from "@/components/ui/sheet";

export const SHORTCUTS: [string, string][] = [
  ["Ctrl/Cmd + K", "Command palette: jump anywhere, run any action"],
  ["Ctrl/Cmd + P", "Switch project"],
  ["Ctrl/Cmd + B", "Show or hide the explorer"],
  ["Ctrl/Cmd + J", "Show or hide the bottom panel"],
  ["Ctrl + W or Alt + W", "Close the current tab"],
  ["Ctrl + Tab or Alt + ]", "Next tab"],
  ["Ctrl + Shift + Tab or Alt + [", "Previous tab"],
  ["?", "Show this list"],
  ["Esc", "Close a dialog or sheet"],
];

export function ShortcutsSheet({ open, onOpenChange }: { open: boolean; onOpenChange: (o: boolean) => void }) {
  useEscapeClose(open, () => onOpenChange(false));
  return (
    <Sheet open={open} onOpenChange={onOpenChange}>
      <SheetContent side="right" className="w-[420px] max-w-[92vw] gap-0 overflow-y-auto sm:max-w-[420px]">
        <SheetHeader>
          <SheetTitle>Keyboard shortcuts</SheetTitle>
          <SheetDescription>Move around without the mouse.</SheetDescription>
        </SheetHeader>
        <dl className="grid grid-cols-[auto_1fr] items-baseline gap-x-4 gap-y-2 px-4 pb-6 text-sm">
          {SHORTCUTS.map(([k, d]) => (
            <div key={k} className="contents">
              <dt>
                <kbd className="rounded border bg-muted px-1.5 py-0.5 font-mono text-xs">{k}</kbd>
              </dt>
              <dd className="text-muted-foreground">{d}</dd>
            </div>
          ))}
        </dl>
      </SheetContent>
    </Sheet>
  );
}
