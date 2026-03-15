"use client";

import { useTheme } from "next-themes";

export function ThemeToggle({ className }: { className?: string }) {
  const { theme, setTheme } = useTheme();
  const dark = theme === "dark";

  return (
    <button
      onClick={() => setTheme(dark ? "light" : "dark")}
      className={`rounded-lg bg-gray-100 p-2 text-zinc-900 transition-colors hover:bg-gray-200 dark:bg-zinc-800 dark:text-zinc-100 dark:hover:bg-zinc-700 ${className || ""}`}
      aria-label="Toggle Theme"
      type="button"
    >
      {dark ? "☀" : "☾"}
    </button>
  );
}
