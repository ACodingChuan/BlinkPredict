"use client";

import { useCallback, useEffect, useRef, useState } from "react";
import api from "@/app/utils/axiosInstance";
import { usePrivy } from "@/lib/auth-client";
import { PositionResponse } from "@/types/market";
import { useCurrentSolanaWallet } from "@/hooks/useCurrentSolanaWallet";

const MUTATION_SYNC_DELAYS_MS = [0, 400, 1200, 2500];

export const useMarketPosition = (marketId: string) => {
  const { authenticated, getAccessToken } = usePrivy();
  const { walletAddress } = useCurrentSolanaWallet();
  const syncJobRef = useRef(0);
  const [loading, setLoading] = useState(false);
  const [position, setPosition] = useState<PositionResponse | null>(null);

  const fetchPosition = useCallback(async () => {
    if (!marketId || !walletAddress || !authenticated) {
      setLoading(false);
      setPosition(null);
      return false;
    }
    const token = await getAccessToken();
    if (!token) {
      setLoading(false);
      setPosition(null);
      return false;
    }
    try {
      const { data } = await api.get<PositionResponse>(`/positions/${marketId}`, {
        headers: { Authorization: `Bearer ${token}` },
      });
      setPosition(data);
    } catch {
      setPosition(null);
    } finally {
      setLoading(false);
    }
    return true;
  }, [authenticated, getAccessToken, marketId, walletAddress]);

  useEffect(() => {
    setLoading(true);
    const timer = setTimeout(() => {
      void fetchPosition();
    }, 0);
    return () => clearTimeout(timer);
  }, [fetchPosition]);

  return {
    position,
    loading,
    refetch: async () => {
      setLoading(true);
      await fetchPosition();
    },
    syncAfterMutation: async () => {
      const jobID = ++syncJobRef.current;
      for (const delayMs of MUTATION_SYNC_DELAYS_MS) {
        if (delayMs > 0) {
          await sleep(delayMs);
        }
        if (jobID !== syncJobRef.current) {
          return;
        }
        await fetchPosition();
      }
    },
  };
};

function sleep(ms: number) {
  return new Promise<void>((resolve) => {
    setTimeout(resolve, ms);
  });
}
