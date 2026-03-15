"use client";

import { useState } from "react";
import Link from "next/link";
import { usePrivy } from "@privy-io/react-auth";
import { useTheme } from "next-themes";
import { toast } from "sonner";
import api from "@/app/utils/axiosInstance";
import { useUSDCStore } from "@/store/usdcStore";
import { getSolanaWalletAddress } from "@/lib/privy";

type OAuthAccount = { type: "google_oauth"; email?: string; name?: string };
type WalletAccount = { type: "wallet"; address?: string };
type EmailAccount = { type: "email"; address?: string };

type BasicAccount = OAuthAccount | WalletAccount | EmailAccount | { type: string };

export const UserMenu = () => {
  const { user, authenticated, logout, getAccessToken } = usePrivy();
  const syncBalance = useUSDCStore((state) => state.syncBalance);
  const { theme, setTheme } = useTheme();
  const [isOpen, setIsOpen] = useState(false);

  if (!user || !authenticated) return null;

  const linkedAccounts = user.linkedAccounts as BasicAccount[];
  const googleAccount = linkedAccounts.find((account): account is OAuthAccount => account.type === "google_oauth");
  const walletAccount = linkedAccounts.find((account): account is WalletAccount => account.type === "wallet");
  const emailAccount = linkedAccounts.find((account): account is EmailAccount => account.type === "email");

  const email = googleAccount?.email || emailAccount?.address;
  const solanaAddress = getSolanaWalletAddress(user as { wallet?: { address?: string; chainType?: string; chain_type?: string }; linkedAccounts?: { type?: string; address?: string; chainType?: string; chain_type?: string }[] } | null);
  const address = solanaAddress || walletAccount?.address || user.wallet?.address;
  const displayName = googleAccount?.name || (address ? `${address.slice(0, 6)}...${address.slice(-4)}` : "User");
  const isAdmin = email === process.env.NEXT_PUBLIC_ADMIN_EMAIL;
  const initial = (googleAccount?.name?.[0] || address?.[0] || "U").toUpperCase();

  const handleFaucet = async () => {
    try {
      const token = await getAccessToken();
      if (!token) {
        toast.error("Not authenticated", { description: "Please login again." });
        return;
      }
      if (!solanaAddress) {
        toast.error("No Solana wallet", { description: "Please connect/link a Solana wallet first." });
        return;
      }
      const { data } = await api.post(
        "/faucet/claim",
        { wallet_address: solanaAddress },
        { headers: { "privy-id-token": token } },
      );
      toast.success("Faucet submitted", { description: data.signature });
      void syncBalance(solanaAddress);
      setIsOpen(false);
    } catch (error: unknown) {
      const response =
        typeof error === "object" && error !== null && "response" in error
          ? (error as { response?: { status?: number; data?: { message?: string; next_allowed_at?: string } } }).response
          : undefined;
      const message = response?.data?.message || (error instanceof Error ? error.message : "Faucet failed");
      const next = response?.data?.next_allowed_at;
      toast.error(message, next ? { description: `Next allowed at: ${next}` } : undefined);
    }
  };

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
            {email ? <p className="mt-0.5 truncate text-xs text-zinc-500">{email}</p> : null}
            {!email && address ? <p className="mt-0.5 truncate font-mono text-xs text-zinc-500">{address}</p> : null}
          </div>
          <div className="py-1">
            <button onClick={handleFaucet} className="flex w-full items-center gap-2 px-4 py-2 text-left text-sm font-semibold text-[#07C285] hover:bg-zinc-50 dark:hover:bg-zinc-800" type="button">Faucet (vUSDC)</button>
            <Link href="/profile" className="flex items-center gap-2 px-4 py-2 text-sm text-zinc-700 hover:bg-zinc-50 dark:text-zinc-300 dark:hover:bg-zinc-800">Profile</Link>
            {isAdmin ? <Link href="/admin/markets" className="flex items-center gap-2 px-4 py-2 text-sm font-medium text-[#07C285] hover:bg-zinc-50 dark:hover:bg-zinc-800">Admin Dashboard</Link> : null}
          </div>
          <div className="mt-1 border-t border-zinc-100 pt-1 dark:border-zinc-800">
            <button onClick={() => setTheme(theme === "dark" ? "light" : "dark")} className="flex w-full items-center gap-2 px-4 py-2 text-left text-sm text-zinc-700 hover:bg-zinc-50 dark:text-zinc-300 dark:hover:bg-zinc-800" type="button">Theme</button>
            <button onClick={() => logout()} className="flex w-full items-center gap-2 px-4 py-2 text-left text-sm text-red-600 hover:bg-red-50 dark:hover:bg-red-900/10" type="button">Log out</button>
          </div>
        </div>
      ) : null}
    </div>
  );
};
