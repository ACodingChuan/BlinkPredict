"use client";

import { useState } from "react";
import { useTrading } from "@/hooks/useTrading";
import { Market } from "@/types/market";

export const SplitMergeModal = ({ isOpen, onClose, type, market }: { isOpen: boolean; onClose: () => void; type: "split" | "merge"; market: Market }) => {
  const [amount, setAmount] = useState("");
  const { placeOrder, loading } = useTrading();

  if (!isOpen) return null;

  return (
    <div className="fixed inset-0 z-50 flex items-center justify-center bg-black/50 p-4 backdrop-blur-sm">
      <div className="w-full max-w-md rounded-3xl border border-zinc-200 bg-white p-6 shadow-2xl dark:border-zinc-800 dark:bg-zinc-900">
        <h3 className="text-xl font-semibold text-zinc-900 dark:text-zinc-100">{type === "split" ? "Split collateral" : "Merge outcome tokens"}</h3>
        <p className="mt-2 text-sm text-zinc-500 dark:text-zinc-400">This v1a flow already targets the new backend endpoints, and safely surfaces pending transaction-builder gaps.</p>
        <input
          className="mt-5 w-full rounded-2xl border border-zinc-200 bg-zinc-50 px-4 py-3 text-lg font-medium outline-none transition focus:border-zinc-900 dark:border-zinc-700 dark:bg-zinc-950 dark:text-zinc-100 dark:focus:border-zinc-100"
          placeholder="Amount in vUSDC"
          type="number"
          value={amount}
          onChange={(event) => setAmount(event.target.value)}
        />
        <div className="mt-6 flex gap-3">
          <button className="flex-1 rounded-2xl bg-zinc-100 px-4 py-3 font-medium text-zinc-700 dark:bg-zinc-800 dark:text-zinc-200" onClick={onClose}>Cancel</button>
          <button
            className="flex-1 rounded-2xl bg-zinc-900 px-4 py-3 font-semibold text-white disabled:opacity-50 dark:bg-zinc-100 dark:text-zinc-900"
            disabled={!amount || loading}
            onClick={async () => {
              const ok = await placeOrder({ market, action: "buy", outcome: "yes", orderType: type, amount, limitPrice: 0.5 });
              if (ok) {
                setAmount("");
                onClose();
              }
            }}
          >
            {loading ? "Working..." : type === "split" ? "Split" : "Merge"}
          </button>
        </div>
      </div>
    </div>
  );
};
