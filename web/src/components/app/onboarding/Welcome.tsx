// The first-run welcome: five short steps (the four ideas, then what to do first). Skippable at any point, reopened from the user menu.
import { FolderGit2, Inbox, ListChecks, MessagesSquare, Sparkles } from "lucide-react";
import { useEffect, useState, type ReactNode } from "react";
import { Button } from "@/components/ui/button";
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from "@/components/ui/dialog";
import { cn } from "@/lib/utils";

interface Idea {
  icon: ReactNode;
  title: string;
  text: string;
}

const IDEAS: Idea[] = [
  { icon: <FolderGit2 className="size-6" />, title: "Project", text: "A repository your agents work in. Sessions, tasks, memory and teammates all live inside a project." },
  { icon: <MessagesSquare className="size-6" />, title: "Session", text: "A conversation with an agent. Ask for something; it reads files, edits them, runs commands and answers." },
  { icon: <ListChecks className="size-6" />, title: "Task and requirement", text: "Work you hand off. An agent does it on its own and opens a pull request for you to review." },
  { icon: <Inbox className="size-6" />, title: "Needs you", text: "The inbox where agents ask: approve an action, answer a question, review a result. Everything else runs without you." },
];

export interface FirstActions {
  newProject: () => void;
  startSession: () => void;
  trySample: () => void;
}

export function Welcome({ open, onClose, actions }: { open: boolean; onClose: () => void; actions: FirstActions }) {
  const [step, setStep] = useState(0);
  useEffect(() => {
    if (open) setStep(0);
  }, [open]);
  const last = IDEAS.length;
  const idea = IDEAS[step];
  const act = (fn: () => void) => () => {
    onClose();
    fn();
  };
  return (
    <Dialog open={open} onOpenChange={(o) => !o && onClose()}>
      <DialogContent className="sm:max-w-md" aria-describedby="welcome-desc">
        <DialogHeader>
          <DialogTitle>{idea ? "Welcome to 7ar" : "Where to start"}</DialogTitle>
          <DialogDescription id="welcome-desc">
            {idea ? `Four ideas to know. Step ${step + 1} of ${IDEAS.length}.` : "Pick one. You can always find these again under Getting started."}
          </DialogDescription>
        </DialogHeader>
        {idea ? (
          <div className="flex items-start gap-4 py-2" key={step}>
            <div className="flex size-12 shrink-0 items-center justify-center rounded-xl bg-accent text-accent-foreground">{idea.icon}</div>
            <div className="space-y-1">
              <h3 className="text-base font-semibold">{idea.title}</h3>
              <p className="text-sm text-muted-foreground">{idea.text}</p>
            </div>
          </div>
        ) : (
          <div className="grid gap-2 py-1">
            <Button variant="outline" className="h-auto justify-start gap-3 whitespace-normal px-3 py-2 text-left" onClick={act(actions.trySample)}>
              <Sparkles className="size-4 shrink-0" />
              <span>
                <span className="block font-medium">Try it with a sample</span>
                <span className="block text-xs font-normal text-muted-foreground">A small project and a first session, ready to go. Takes a minute.</span>
              </span>
            </Button>
            <Button variant="outline" className="h-auto justify-start gap-3 whitespace-normal px-3 py-2 text-left" onClick={act(actions.newProject)}>
              <FolderGit2 className="size-4 shrink-0" />
              <span>
                <span className="block font-medium">Create or import a project</span>
                <span className="block text-xs font-normal text-muted-foreground">Start from a template or bring a repository from GitHub.</span>
              </span>
            </Button>
            <Button variant="outline" className="h-auto justify-start gap-3 whitespace-normal px-3 py-2 text-left" onClick={act(actions.startSession)}>
              <MessagesSquare className="size-4 shrink-0" />
              <span>
                <span className="block font-medium">Start a session</span>
                <span className="block text-xs font-normal text-muted-foreground">Chat with an agent right away, no project needed.</span>
              </span>
            </Button>
          </div>
        )}
        <div className="flex justify-center gap-1.5" aria-hidden>
          {Array.from({ length: IDEAS.length + 1 }, (_, i) => (
            <span key={i} className={cn("size-1.5 rounded-full", i === step ? "bg-primary" : "bg-muted-foreground/30")} />
          ))}
        </div>
        <DialogFooter className="gap-2 sm:justify-between">
          <Button variant="ghost" onClick={onClose}>
            {step === last ? "Close" : "Skip for now"}
          </Button>
          <div className="flex gap-2">
            {step > 0 ? (
              <Button variant="outline" onClick={() => setStep(step - 1)}>
                Back
              </Button>
            ) : null}
            {step < last ? (
              <Button onClick={() => setStep(step + 1)} autoFocus>
                {step === last - 1 ? "Where to start" : "Next"}
              </Button>
            ) : null}
          </div>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
