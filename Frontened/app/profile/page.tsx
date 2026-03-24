"use client";

import Link from "next/link";
import { usePrivy } from "@privy-io/react-auth";
import { useUSDCBalance } from "@/hooks/useUSDCBalance";
import { getSolanaWalletAddress } from "@/lib/privy";

type LinkedAccount = { type: string; email?: string; address?: string };

export default function ProfilePage() {
  const { user, login } = usePrivy();
  const { balance, totalBalance } = useUSDCBalance();
  const solanaAddress = getSolanaWalletAddress(user as { wallet?: { address?: string; chainType?: string; chain_type?: string }; linkedAccounts?: { type?: string; address?: string; chainType?: string; chain_type?: string }[] } | null);
  const primaryAccount = user ? getPrimaryAccount(user.linkedAccounts as LinkedAccount[]) : "Wallet-first account";

  return (
    <div className="mx-auto max-w-4xl px-4 py-10 sm:px-6 lg:px-8">
      <Link href="/" className="text-sm font-medium text-zinc-500 hover:text-zinc-900 dark:text-zinc-400 dark:hover:text-zinc-100">← Back</Link>
      <section className="mt-6 rounded-[2rem] border border-black/5 bg-white p-8 shadow-sm dark:border-white/10 dark:bg-zinc-900">
        <h1 className="text-3xl font-semibold tracking-tight text-zinc-950 dark:text-zinc-50">Portfolio Shell</h1>
        {!user ? (
          <div className="mt-6 rounded-2xl border border-dashed border-zinc-300 px-6 py-10 text-center dark:border-zinc-700">
            <p className="text-zinc-500 dark:text-zinc-400">Connect your wallet to inspect the v1a position shell.</p>
            <button className="mt-4 rounded-full bg-zinc-900 px-5 py-3 text-sm font-semibold text-white dark:bg-zinc-100 dark:text-zinc-900" onClick={login}>Connect</button>
          </div>
        ) : (
          <div className="mt-6 grid gap-4 md:grid-cols-2">
            <Info label="Wallet" value={solanaAddress || "No Solana wallet linked"} mono />
            <Info label="Trading total" value={`${totalBalance} vUSDC`} />
            <Info label="Trading available" value={`${balance} vUSDC`} />
            <Info label="v1a status" value="Position and claim flows are scaffolded; matching-led fills arrive in v1b." />
            <Info label="Primary account" value={primaryAccount} />
          </div>
        )}
      </section>
    </div>
  );
}

function getPrimaryAccount(accounts: LinkedAccount[]): string {
  const google = accounts.find((account) => account.type === "google_oauth" && account.email);
  if (google?.email) return google.email;
  const email = accounts.find((account) => account.type === "email" && account.address);
  if (email?.address) return email.address;
  return "Wallet-first account";
}

const Info = ({ label, value, mono = false }: { label: string; value: string; mono?: boolean }) => (
  <div className="rounded-2xl bg-zinc-50 p-5 dark:bg-zinc-800/60">
    <div className="text-xs uppercase tracking-[0.2em] text-zinc-500 dark:text-zinc-400">{label}</div>
    <div className={`mt-2 text-sm text-zinc-900 dark:text-zinc-100 ${mono ? "break-all font-mono" : "leading-6"}`}>{value}</div>
  </div>
);
