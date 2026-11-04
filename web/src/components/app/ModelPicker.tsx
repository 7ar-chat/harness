import { Check, ChevronsUpDown, Eye, Brain, Star, Wrench } from "lucide-react";
import { useCallback, useMemo, useState } from "react";
import type { ModelInfo } from "@/api/generated";
import { Button } from "@/components/ui/button";
import { Command, CommandEmpty, CommandGroup, CommandInput, CommandItem, CommandList } from "@/components/ui/command";
import { Popover, PopoverContent, PopoverTrigger } from "@/components/ui/popover";
import { cn } from "@/lib/utils";

// The ids stay exactly what the server (or a router such as 9router, "cc/claude-opus-5-5") calls
// them: they are what is sent back on a run. Only the display is derived here: the vendor
// (category), a readable name, and the route prefix.
type Vendor = { label: string; letter: string; color: string };
const VENDORS: (Vendor & { match: RegExp })[] = [
  { match: /claude|haiku|sonnet|opus/i, label: "Anthropic", letter: "A", color: "bg-[#d97757]" },
  { match: /deepseek/i, label: "DeepSeek", letter: "D", color: "bg-[#4d6bfe]" },
  { match: /gemini|gemma/i, label: "Google", letter: "G", color: "bg-[#1a73e8]" },
  { match: /gpt|^o\d|codex|openai/i, label: "OpenAI", letter: "O", color: "bg-[#10a37f]" },
  { match: /qwen/i, label: "Qwen", letter: "Q", color: "bg-[#6f42c1]" },
  { match: /glm|zhipu/i, label: "Z.ai", letter: "Z", color: "bg-[#2f6bff]" },
  { match: /kimi|moonshot/i, label: "Moonshot", letter: "K", color: "bg-[#111827]" },
  { match: /grok/i, label: "xAI", letter: "X", color: "bg-[#111827]" },
  { match: /llama/i, label: "Meta", letter: "M", color: "bg-[#0866ff]" },
  { match: /mistral|codestral/i, label: "Mistral", letter: "M", color: "bg-[#fa520f]" },
  { match: /minimax/i, label: "MiniMax", letter: "m", color: "bg-[#e11d48]" },
];
const OTHER: Vendor = { label: "Other", letter: "•", color: "bg-muted-foreground" };
const CASE: Record<string, string> = { gpt: "GPT", deepseek: "DeepSeek", glm: "GLM", gemini: "Gemini", claude: "Claude", qwen: "Qwen", kimi: "Kimi", llama: "Llama", grok: "Grok", mistral: "Mistral", minimax: "MiniMax", oss: "OSS", vl: "VL" };

export function modelParts(id: string): { route: string; vendor: Vendor; name: string } {
  const slash = id.lastIndexOf("/");
  const route = slash > 0 ? id.slice(0, slash) : "";
  const raw = slash >= 0 ? id.slice(slash + 1) : id;
  const vendor = VENDORS.find((v) => v.match.test(raw)) ?? OTHER;
  const out: string[] = [];
  for (const t of raw.split(/[-_:\s]+/).filter(Boolean)) {
    const prev = out[out.length - 1];
    // "5" "5" -> "5.5", "4" "1" -> "4.1" (but not an 8-digit date)
    if (/^\d{1,2}$/.test(t) && prev && /^v?\d{1,2}(\.\d+)*$/i.test(prev) && prev.length < 4) out[out.length - 1] = `${prev}.${t}`;
    else out.push(t);
  }
  const name = out.map((t) => CASE[t.toLowerCase()] ?? (/^\d/.test(t) ? t : /^v\d/i.test(t) ? t.toUpperCase() : t[0]!.toUpperCase() + t.slice(1))).join(" ");
  return { route, vendor, name };
}

export function VendorIcon({ vendor, className }: { vendor: Vendor; className?: string }) {
  return (
    <span aria-hidden className={cn("inline-flex size-4 shrink-0 items-center justify-center rounded text-[10px] font-bold leading-none text-white", vendor.color, className)}>
      {vendor.letter}
    </span>
  );
}

const KEY = "harness.starredModels";
function loadStars(): string[] {
  try {
    const v = JSON.parse(localStorage.getItem(KEY) ?? "[]");
    return Array.isArray(v) ? v.filter((x): x is string => typeof x === "string") : [];
  } catch {
    return [];
  }
}

function Badges({ info }: { info?: ModelInfo }) {
  if (!info) return null;
  return (
    <span className="flex shrink-0 items-center gap-1 text-muted-foreground">
      {info.reasoning ? <Brain className="size-3.5" aria-label="Reasoning" /> : null}
      {info.vision ? <Eye className="size-3.5" aria-label="Vision" /> : null}
      {info.tools ? <Wrench className="size-3.5" aria-label="Tools" /> : null}
      {info.contextWindow ? <span className="text-[10px]">{info.contextWindow >= 1e6 ? `${info.contextWindow / 1e6}M` : `${Math.round(info.contextWindow / 1000)}k`}</span> : null}
    </span>
  );
}

export function ModelPicker({ models, value, onChange, info, disabled, placeholder = "Model" }: { models: string[]; value?: string; onChange: (id: string) => void; info?: Record<string, ModelInfo>; disabled?: boolean; placeholder?: string }) {
  const [open, setOpen] = useState(false);
  const [stars, setStars] = useState(loadStars);
  const [onlyStars, setOnlyStars] = useState(true);
  const toggle = useCallback((id: string) => {
    setStars((cur) => {
      const next = cur.includes(id) ? cur.filter((x) => x !== id) : [...cur, id];
      try {
        localStorage.setItem(KEY, JSON.stringify(next));
      } catch {
        /* storage unavailable: the stars last until reload */
      }
      return next;
    });
  }, []);
  const starred = models.filter((m) => stars.includes(m));
  const filtering = onlyStars && starred.length > 0;
  const shown = filtering ? models.filter((m) => stars.includes(m) || m === value) : models;
  const groups = useMemo(() => {
    const m = new Map<string, { vendor: Vendor; ids: string[] }>();
    for (const id of shown) {
      const { vendor } = modelParts(id);
      const g = m.get(vendor.label) ?? { vendor, ids: [] };
      g.ids.push(id);
      m.set(vendor.label, g);
    }
    return [...m.values()].sort((a, b) => (a.vendor === OTHER ? 1 : b.vendor === OTHER ? -1 : a.vendor.label.localeCompare(b.vendor.label)));
  }, [shown]);
  const cur = value ? modelParts(value) : null;
  return (
    <Popover open={open} onOpenChange={setOpen}>
      <PopoverTrigger asChild>
        <Button variant="outline" size="sm" role="combobox" aria-expanded={open} aria-label="Model" disabled={disabled} className="h-7 w-56 justify-between px-2 text-xs font-normal">
          <span className="flex min-w-0 items-center gap-1.5">
            {cur ? <VendorIcon vendor={cur.vendor} /> : null}
            <span className="truncate">{cur ? cur.name : placeholder}</span>
          </span>
          <ChevronsUpDown className="size-3.5 shrink-0 opacity-50" />
        </Button>
      </PopoverTrigger>
      <PopoverContent align="start" className="w-96 p-0">
        <Command>
          <CommandInput placeholder={`Search ${models.length} models…`} />
          <div className="flex items-center justify-between border-b px-3 py-1.5 text-xs text-muted-foreground">
            <span>{filtering ? `${starred.length} starred` : starred.length ? "All models" : "Star models to keep a short list"}</span>
            {starred.length ? (
              <button type="button" className="underline-offset-2 hover:underline" onClick={() => setOnlyStars((v) => !v)}>
                {onlyStars ? "Show all" : "Starred only"}
              </button>
            ) : null}
          </div>
          <CommandList className="max-h-80">
            <CommandEmpty>No model matches.</CommandEmpty>
            {groups.map(({ vendor, ids }) => (
              <CommandGroup key={vendor.label} heading={`${vendor.label} · ${ids.length}`}>
                {ids.map((id) => {
                  const { name, route } = modelParts(id);
                  const on = stars.includes(id);
                  return (
                    <CommandItem
                      key={id}
                      value={id}
                      onSelect={() => {
                        onChange(id);
                        setOpen(false);
                      }}
                    >
                      <VendorIcon vendor={vendor} />
                      <span className="truncate">{name}</span>
                      {route ? <span className="shrink-0 rounded bg-muted px-1 text-[10px] text-muted-foreground">{route}</span> : null}
                      <span className="ml-auto flex items-center gap-1.5">
                        <Badges info={info?.[id]} />
                        {id === value ? <Check className="size-4 shrink-0" /> : null}
                        <button
                          type="button"
                          aria-label={on ? `Unstar ${name}` : `Star ${name}`}
                          aria-pressed={on}
                          className="rounded p-0.5 hover:bg-accent"
                          onClick={(e) => {
                            e.stopPropagation();
                            toggle(id);
                          }}
                          onPointerDown={(e) => e.stopPropagation()}
                        >
                          <Star className={cn("size-4", on ? "fill-yellow-400 text-yellow-500" : "text-muted-foreground")} />
                        </button>
                      </span>
                    </CommandItem>
                  );
                })}
              </CommandGroup>
            ))}
          </CommandList>
        </Command>
      </PopoverContent>
    </Popover>
  );
}
