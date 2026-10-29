import { DiffModeEnum, DiffView } from "@git-diff-view/react";
import "@git-diff-view/react/styles/diff-view-pure.css";
import { createTwoFilesPatch } from "diff";
import { useMemo } from "react";
import { useTheme } from "next-themes";

export function DiffBlock({ path, oldText, newText }: { path: string; oldText: string; newText: string }) {
  const { resolvedTheme } = useTheme();
  const hunk = useMemo(() => createTwoFilesPatch(path || "file", path || "file", oldText, newText, "", "", { context: 3 }), [path, oldText, newText]);
  return (
    <div className="max-h-96 overflow-auto rounded-md border text-xs">
      <DiffView
        data={{ oldFile: { fileName: path, content: oldText }, newFile: { fileName: path, content: newText }, hunks: [hunk] }}
        diffViewMode={DiffModeEnum.Unified}
        diffViewTheme={resolvedTheme === "dark" ? "dark" : "light"}
        diffViewHighlight
        diffViewWrap
        diffViewFontSize={12}
      />
    </div>
  );
}
