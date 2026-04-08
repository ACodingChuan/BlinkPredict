"use client";

import Link from "next/link";
import { useState } from "react";
import { usePrivy } from "@/lib/auth-client";

export const UserMenu = () => {
  const { user, authenticated, logout } = usePrivy();
  const [isOpen, setIsOpen] = useState(false);

  if (!user || !authenticated) return null;

  const address = user.walletAddress;
  const displayName = `${address.slice(0, 6)}...${address.slice(-4)}`;
  const initial = address[0]?.toUpperCase() || "U";

  return (
    <div className="relative">
      <button
        onClick={() => setIsOpen((open) => !open)}
        className="flex items-center gap-2 rounded-lg border border-transparent p-1.5 transition-colors hover:border-zinc-200 hover:bg-zinc-100 dark:hover:border-zinc-700 dark:hover:bg-zinc-800"
        type="button"
      >
        <div className="flex h-8 w-8 items-center justify-center rounded-full bg-gradient-to-br from-[#07C285] to-teal-600 text-sm font-bold text-white shadow-sm">
          {initial}
        </div>
        <span className="hidden max-w-[120px] truncate text-sm font-medium text-zinc-700 dark:text-zinc-200 md:block">{displayName}</span>
      </button>

      {isOpen ? (
        <div className="absolute right-0 top-full z-50 mt-2 w-60 overflow-hidden rounded-xl border border-zinc-200 bg-white py-2 shadow-xl dark:border-zinc-800 dark:bg-zinc-900">
          <div className="border-b border-zinc-100 px-4 py-3 dark:border-zinc-800">
            <p className="truncate text-sm font-bold text-zinc-900 dark:text-white">{displayName}</p>
            <p className="mt-0.5 truncate font-mono text-xs text-zinc-500">{address}</p>
          </div>
          {user.isAdmin ? (
            <div className="py-1">
              <Link href="/admin/markets" className="flex items-center gap-2 px-4 py-2 text-sm font-medium text-[#07C285] hover:bg-zinc-50 dark:hover:bg-zinc-800">Admin Dashboard</Link>
            </div>
          ) : null}
          <div className="mt-1 border-t border-zinc-100 pt-1 dark:border-zinc-800">
            <button onClick={() => void logout()} className="flex w-full items-center gap-2 px-4 py-2 text-left text-sm text-red-600 hover:bg-red-50 dark:hover:bg-red-900/10" type="button">Log out</button>
          </div>
        </div>
      ) : null}
    </div>
  );
};
