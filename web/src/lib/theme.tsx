import { ThemeProvider as NextThemes, useTheme } from "next-themes";
import type { ReactNode } from "react";

export const ThemeProvider = ({ children }: { children: ReactNode }) => (
  <NextThemes attribute="class" defaultTheme="system" enableSystem>
    {children}
  </NextThemes>
);
export { useTheme };
