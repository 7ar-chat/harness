import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { RouterProvider } from "@tanstack/react-router";
import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import { loginRedirect } from "@/api/client";
import { Toaster } from "@/components/ui/sonner";
import { TooltipProvider } from "@/components/ui/tooltip";
import { unlockAudioOnGesture } from "@/lib/sound";
import { ThemeProvider } from "@/lib/theme";
import { loadClientConfig, startAnalytics, startErrorReporting } from "@/lib/telemetry";
import { router } from "./router";
import "./index.css";

const queryClient = new QueryClient({ defaultOptions: { queries: { refetchOnWindowFocus: true, staleTime: 10_000, retry: 1 } } });

unlockAudioOnGesture();

// Error reporting and usage statistics are optional and configured by the server. The page does not wait for the
// answer: it renders at once and the reporters start a moment later.
void loadClientConfig().then((cfg) => {
  startErrorReporting(cfg.sentry);
  startAnalytics(cfg.umami);
});

// A 401 from any call means the session is gone: go through the server's OIDC login and come back.
window.addEventListener("harness:unauthorized", () => setTimeout(loginRedirect, 800));

createRoot(document.getElementById("root")!).render(
  <StrictMode>
    <ThemeProvider>
      <QueryClientProvider client={queryClient}>
        <TooltipProvider>
          <RouterProvider router={router} />
          <Toaster richColors closeButton />
        </TooltipProvider>
      </QueryClientProvider>
    </ThemeProvider>
  </StrictMode>,
);
