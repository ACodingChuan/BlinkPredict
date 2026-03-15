"use client";

import { useEffect, useState } from "react";
import api from "@/app/utils/axiosInstance";
import { useIdentityToken, usePrivy } from "@privy-io/react-auth";

export const UserOpenOrders = ({ marketId }: { marketId: number }) => {
  const { user } = usePrivy();
  const { identityToken } = useIdentityToken();
  const [matchingEnabled, setMatchingEnabled] = useState(false);

  useEffect(() => {
    const load = async () => {
      if (!user) return;
      const { data } = await api.get<{ orders: unknown[]; matching_enabled: boolean }>(`/orders/open/${marketId}`, {
        headers: { "privy-id-token": identityToken },
      });
      setMatchingEnabled(data.matching_enabled);
    };
    load().catch(console.error);
  }, [marketId, user, identityToken]);

  return (
    <div className="rounded-3xl border border-dashed border-zinc-300 px-4 py-8 text-center text-sm text-zinc-500 dark:border-zinc-700 dark:text-zinc-400">
      {user ? (matchingEnabled ? "Open orders will appear here." : "Open orders will appear here once v1b matching is enabled.") : "Connect your wallet to view open orders."}
    </div>
  );
};
