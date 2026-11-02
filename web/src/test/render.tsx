import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { RouterProvider } from "@tanstack/react-router";
import { render, waitFor } from "@testing-library/react";
import { expect } from "vitest";
import { TooltipProvider } from "@/components/ui/tooltip";
import { ThemeProvider } from "@/lib/theme";
import { router } from "@/router";

export const newQueryClient = () => new QueryClient({ defaultOptions: { queries: { retry: false, staleTime: 0 }, mutations: { retry: false } } });

/** Renders the real app (providers as in main.tsx) at a hash route. */
export async function renderApp(to: string, qc = newQueryClient()) {
  const utils = render(
    <ThemeProvider>
      <QueryClientProvider client={qc}>
        <TooltipProvider>
          <RouterProvider router={router} />
        </TooltipProvider>
      </QueryClientProvider>
    </ThemeProvider>,
  );
  // The router is a module singleton shared by every test: mount it, then move it to the route.
  await router.navigate({ href: to });
  await waitFor(() => expect(router.state.status).toBe("idle"));
  return { qc, router, ...utils };
}
