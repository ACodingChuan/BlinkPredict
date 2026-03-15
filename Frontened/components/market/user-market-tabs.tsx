"use client";

import { UserOpenOrders } from "./user-open-orders";

export const UserMarketTabs = ({ marketId }: { marketId: number }) => {
  return (
    <section className="rounded-3xl border border-zinc-200 bg-white p-6 shadow-sm dark:border-zinc-800 dark:bg-zinc-900">
      <h3 className="text-lg font-semibold text-zinc-900 dark:text-zinc-100">Your Open Orders</h3>
      <p className="mt-1 text-sm text-zinc-500 dark:text-zinc-400">The UI contract is already in place; v1a simply returns an empty matching-disabled state.</p>
      <div className="mt-5">
        <UserOpenOrders marketId={marketId} />
      </div>
    </section>
  );
};
