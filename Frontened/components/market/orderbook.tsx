"use client";

import { useEffect, useState } from "react";
import api from "@/app/utils/axiosInstance";
import { OrderbookSnapshot } from "@/types/market";

interface OrderbookProps {
  outcome: "yes" | "no";
  marketId: number;
}

const EMPTY_BOOK: OrderbookSnapshot = {
  yes: { bids: [], asks: [] },
  no: { bids: [], asks: [] },
  matching_enabled: false,
};

export const Orderbook = ({ outcome, marketId }: OrderbookProps) => {
  const [snapshot, setSnapshot] = useState<OrderbookSnapshot>(EMPTY_BOOK);

  useEffect(() => {
    const fetchOrderbook = async () => {
      try {
        const { data } = await api.get<OrderbookSnapshot>(`/orderbook/${marketId}`);
        setSnapshot(data);
      } catch (error) {
        console.error("Failed to fetch orderbook", error);
      }
    };

    fetchOrderbook();
  }, [marketId]);

  const book = snapshot[outcome];

  return (
    <section className="rounded-3xl border border-zinc-200 bg-white p-6 shadow-sm dark:border-zinc-800 dark:bg-zinc-900">
      <div className="mb-4 flex items-center justify-between">
        <div>
          <h3 className="text-lg font-semibold text-zinc-900 dark:text-zinc-100">{outcome.toUpperCase()} Order Book</h3>
          <p className="text-sm text-zinc-500 dark:text-zinc-400">v1a returns the contract-compatible empty book while matching is disabled.</p>
        </div>
        <span className="rounded-full bg-amber-100 px-3 py-1 text-xs font-semibold text-amber-800 dark:bg-amber-900/40 dark:text-amber-200">
          matching {snapshot.matching_enabled ? "enabled" : "disabled"}
        </span>
      </div>
      <div className="grid grid-cols-2 gap-6">
        <BookColumn title="Bids" rows={book.bids} accent="text-emerald-600 dark:text-emerald-400" />
        <BookColumn title="Asks" rows={book.asks} accent="text-rose-600 dark:text-rose-400" />
      </div>
    </section>
  );
};

const BookColumn = ({ title, rows, accent }: { title: string; rows: Array<{ price: string; quantity: string }>; accent: string }) => (
  <div>
    <div className="mb-2 flex justify-between border-b border-zinc-200 pb-2 text-xs font-semibold uppercase tracking-wide text-zinc-400 dark:border-zinc-800">
      <span>{title}</span>
      <span>Quantity</span>
    </div>
    {rows.length === 0 ? (
      <div className="rounded-2xl border border-dashed border-zinc-200 px-4 py-8 text-center text-sm text-zinc-500 dark:border-zinc-800 dark:text-zinc-400">
        No levels yet.
      </div>
    ) : (
      <div className="space-y-2">
        {rows.map((row, index) => (
          <div key={`${title}-${index}`} className="flex items-center justify-between rounded-2xl bg-zinc-50 px-3 py-2 text-sm dark:bg-zinc-800/60">
            <span className={`font-semibold ${accent}`}>{row.price}</span>
            <span className="font-mono text-zinc-600 dark:text-zinc-300">{row.quantity}</span>
          </div>
        ))}
      </div>
    )}
  </div>
);
