"use client";

import Link from "next/link";
import { useState } from "react";
import { ThemeToggle } from "@/components/theme-toggle";
import { UserMenu } from "@/components/user-menu";
import { usePrivy } from "@/lib/auth-client";
import { useUSDCBalance } from "@/hooks/useUSDCBalance";
import { TradingTransferKind, useTradingTransfers } from "@/hooks/useTradingTransfers";

export const AppHeader = () => {
  const { user, login, authenticated } = usePrivy();
  const { availableBalance, lockedBalance, pendingBalance, loading, refetch } = useUSDCBalance();
  const { busyKind, submitTransfer } = useTradingTransfers();
  const [openKind, setOpenKind] = useState<TradingTransferKind | null>(null);
  const [amount, setAmount] = useState("");

  const openTransfer = (kind: TradingTransferKind) => {
    setAmount("");
    setOpenKind(kind);
  };

  return (
    <>
      <header className="sticky top-0 z-40 border-b border-black/5 bg-white/80 backdrop-blur-xl dark:border-white/10 dark:bg-zinc-950/80">
        <div className="flex w-full flex-wrap items-center gap-4 px-4 py-4 sm:px-6 lg:px-10">
          <div className="min-w-0">
            <Link href="/" className="block">
              <div className="text-xs uppercase tracking-[0.3em] text-emerald-700 dark:text-emerald-300">SolPredict</div>
              <div className="truncate text-lg font-semibold text-zinc-950 dark:text-zinc-50">Trading Account</div>
            </Link>
          </div>
          <div className="ml-auto flex flex-1 flex-wrap items-center justify-end gap-2">
            {authenticated && user ? (
              <>
                <button
                  aria-label="Refresh trading balances"
                  className="inline-flex h-10 w-10 items-center justify-center rounded-full border border-zinc-300 bg-white text-zinc-700 transition hover:border-zinc-900 hover:text-zinc-950 disabled:cursor-not-allowed disabled:opacity-50 dark:border-zinc-700 dark:bg-zinc-900 dark:text-zinc-200 dark:hover:border-zinc-200 dark:hover:text-zinc-50"
                  disabled={loading}
                  onClick={() => void refetch()}
                  type="button"
                >
                  <RefreshIcon spinning={loading} />
                </button>
                <HeaderBalance label="Available" value={`${availableBalance} vUSDC`} tone="strong" />
                <HeaderBalance label="Locked" value={`${lockedBalance} vUSDC`} />
                <HeaderBalance label="Pending" value={`${pendingBalance} vUSDC`} />
                <button
                  className="rounded-full border border-emerald-600 px-4 py-2 text-sm font-semibold text-emerald-700 disabled:opacity-50 dark:border-emerald-400 dark:text-emerald-300"
                  disabled={busyKind !== null}
                  onClick={() => openTransfer("deposit")}
                  type="button"
                >
                  Deposit
                </button>
                <button
                  className="rounded-full border border-zinc-300 px-4 py-2 text-sm font-semibold text-zinc-700 disabled:opacity-50 dark:border-zinc-700 dark:text-zinc-200"
                  disabled={busyKind !== null}
                  onClick={() => openTransfer("withdraw")}
                  type="button"
                >
                  Withdraw
                </button>
              </>
            ) : null}
            <ThemeToggle />
            {authenticated && user ? (
              <UserMenu />
            ) : (
              <button
                className="rounded-full bg-zinc-900 px-4 py-2 text-sm font-semibold text-white dark:bg-zinc-100 dark:text-zinc-900"
                onClick={() => void login()}
                type="button"
              >
                Connect
              </button>
            )}
          </div>
        </div>
      </header>
      <TransferModal
        amount={amount}
        busy={busyKind === openKind && openKind !== null}
        isOpen={openKind !== null}
        kind={openKind}
        onAmountChange={setAmount}
        onClose={() => {
          if (busyKind === null) {
            setOpenKind(null);
          }
        }}
        onSubmit={async () => {
          if (!openKind) return;
          const ok = await submitTransfer(openKind, amount);
          if (ok) {
            setAmount("");
            setOpenKind(null);
          }
        }}
      />
    </>
  );
};

const HeaderBalance = ({
  label,
  value,
  tone = "default",
}: {
  label: string;
  value: string;
  tone?: "default" | "strong";
}) => (
  <div
    className={`rounded-full px-3 py-1.5 text-xs font-semibold ${
      tone === "strong"
        ? "bg-zinc-900 text-white dark:bg-zinc-100 dark:text-zinc-900"
        : "border border-zinc-300 text-zinc-700 dark:border-zinc-700 dark:text-zinc-200"
    }`}
  >
    {label} {value}
  </div>
);

const RefreshIcon = ({ spinning }: { spinning: boolean }) => (
  <svg
    aria-hidden="true"
    className={`h-4 w-4 ${spinning ? "animate-spin" : ""}`}
    fill="none"
    viewBox="0 0 24 24"
  >
    <path
      d="M20 12a8 8 0 1 1-2.34-5.66"
      stroke="currentColor"
      strokeLinecap="round"
      strokeLinejoin="round"
      strokeWidth="1.8"
    />
    <path
      d="M20 4v6h-6"
      stroke="currentColor"
      strokeLinecap="round"
      strokeLinejoin="round"
      strokeWidth="1.8"
    />
  </svg>
);

const TransferModal = ({
  isOpen,
  kind,
  amount,
  busy,
  onAmountChange,
  onClose,
  onSubmit,
}: {
  isOpen: boolean;
  kind: TradingTransferKind | null;
  amount: string;
  busy: boolean;
  onAmountChange: (value: string) => void;
  onClose: () => void;
  onSubmit: () => Promise<void>;
}) => {
  if (!isOpen || !kind) {
    return null;
  }

  return (
    <div className="fixed inset-0 z-50 flex items-center justify-center bg-black/45 p-4 backdrop-blur-sm">
      <div className="w-full max-w-md rounded-3xl border border-zinc-200 bg-white p-6 shadow-2xl dark:border-zinc-800 dark:bg-zinc-900">
        <div className="text-xs font-semibold uppercase tracking-[0.3em] text-emerald-700 dark:text-emerald-300">
          {kind === "deposit" ? "Deposit" : "Withdraw"}
        </div>
        <h3 className="mt-3 text-2xl font-semibold text-zinc-900 dark:text-zinc-100">
          {kind === "deposit" ? "Move vUSDC into trading account" : "Move vUSDC back to wallet"}
        </h3>
        <input
          className="mt-5 w-full rounded-2xl border border-zinc-200 bg-zinc-50 px-4 py-3 text-lg font-medium outline-none transition focus:border-zinc-900 dark:border-zinc-700 dark:bg-zinc-950 dark:text-zinc-100 dark:focus:border-zinc-100"
          inputMode="decimal"
          min="0"
          onChange={(event) => onAmountChange(event.target.value)}
          placeholder="Amount in vUSDC"
          type="number"
          value={amount}
        />
        <div className="mt-6 flex gap-3">
          <button
            className="flex-1 rounded-2xl bg-zinc-100 px-4 py-3 font-medium text-zinc-700 disabled:opacity-50 dark:bg-zinc-800 dark:text-zinc-200"
            disabled={busy}
            onClick={onClose}
            type="button"
          >
            Cancel
          </button>
          <button
            className="flex-1 rounded-2xl bg-zinc-900 px-4 py-3 font-semibold text-white disabled:opacity-50 dark:bg-zinc-100 dark:text-zinc-900"
            disabled={!amount || busy}
            onClick={() => void onSubmit()}
            type="button"
          >
            {busy ? "Submitting..." : kind === "deposit" ? "Deposit" : "Withdraw"}
          </button>
        </div>
      </div>
    </div>
  );
};
