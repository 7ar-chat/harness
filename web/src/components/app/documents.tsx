import { Download, FileText, Trash2, Upload } from "lucide-react";
import { useRef } from "react";
import { downloadText, isUnsupported } from "@/api/client";
import { toastError, useDeleteDocument, useDocuments, useUploadDocument } from "@/api/queries";
import { Spinner } from "@/components/app/common";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";

const ACCEPT = ".pdf,.doc,.docx,.odt,.rtf,.txt,.md,.csv,.json,.xlsx,.pptx,.png,.jpg,.jpeg,.webp";

const size = (n: number) => (n >= 1 << 20 ? `${(n / (1 << 20)).toFixed(1)} MB` : n >= 1 << 10 ? `${Math.round(n / (1 << 10))} KB` : `${n} B`);

/** The user's document library: files every session and agent of theirs can use (a CV, a cover-letter template...). */
export function DocumentsCard() {
  const docs = useDocuments();
  const upload = useUploadDocument();
  const remove = useDeleteDocument();
  const input = useRef<HTMLInputElement>(null);
  const unsupported = docs.error && isUnsupported(docs.error);
  return (
    <Card>
      <CardHeader>
        <CardTitle className="flex items-center gap-2">
          <FileText className="size-4" /> Documents
        </CardTitle>
        <CardDescription>
          Files your assistant can use in every session: your CV, a cover-letter template, certificates. It reads them to answer for you, and can attach
          them to a web form. Only you see them.
        </CardDescription>
      </CardHeader>
      <CardContent className="space-y-4">
        {unsupported ? (
          <p className="text-sm text-muted-foreground">This server keeps no document library.</p>
        ) : (
          <>
            <input
              ref={input}
              type="file"
              multiple
              accept={ACCEPT}
              className="sr-only"
              aria-label="Choose documents to add"
              onChange={(e) => {
                const files = Array.from(e.target.files ?? []);
                e.target.value = "";
                for (const f of files) upload.mutate(f);
              }}
            />
            <Button variant="outline" onClick={() => input.current?.click()} disabled={upload.isPending}>
              {upload.isPending ? <Spinner /> : <Upload />} Add documents
            </Button>
            {docs.isLoading ? <Spinner /> : null}
            {docs.data && docs.data.length === 0 ? <p className="text-sm text-muted-foreground">No documents yet. PDF, Word, text, spreadsheet, slides or image, up to 20 MB each.</p> : null}
            <ul className="divide-y rounded-md border" aria-label="Your documents">
              {(docs.data ?? []).map((d) => (
                <li key={d.name} className="flex items-center gap-3 px-3 py-2 text-sm">
                  <FileText className="size-4 shrink-0 text-muted-foreground" />
                  <span className="min-w-0 flex-1 truncate" title={d.name}>
                    {d.name}
                  </span>
                  <span className="shrink-0 text-xs text-muted-foreground">{size(d.size)}</span>
                  <Button
                    variant="ghost"
                    size="icon"
                    aria-label={`Download ${d.name}`}
                    onClick={() => void downloadText(`/me/documents/${encodeURIComponent(d.name)}`, d.name).catch(toastError)}
                  >
                    <Download />
                  </Button>
                  <Button variant="ghost" size="icon" aria-label={`Delete ${d.name}`} disabled={remove.isPending} onClick={() => confirm(`Delete ${d.name}?`) && remove.mutate(d.name)}>
                    <Trash2 />
                  </Button>
                </li>
              ))}
            </ul>
          </>
        )}
      </CardContent>
    </Card>
  );
}
