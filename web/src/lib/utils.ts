import { clsx, type ClassValue } from "clsx";
import { twMerge } from "tailwind-merge";

/** Reads a Blob as a data: URL. The CSP allows data: images but not blob: ones. */
export const blobToDataUrl = (b: Blob) =>
  new Promise<string>((res, rej) => {
    const r = new FileReader();
    r.onload = () => res(String(r.result));
    r.onerror = () => rej(r.error);
    r.readAsDataURL(b);
  });

export function cn(...inputs: ClassValue[]) {
  return twMerge(clsx(inputs));
}
